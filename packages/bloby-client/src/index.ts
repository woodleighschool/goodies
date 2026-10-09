import { Crc64Nvme } from "@aws-sdk/crc64-nvme";
import { SHA256 } from "@stablelib/sha256";
import pMap from "p-map";
import pRetry from "p-retry";
import { uint8ArrayToHex } from "uint8array-extras";

export interface UploadProgress {
  loaded: number;
  total: number;
  percent: number;
}

export interface UploadTarget {
  url: string;
  method: "PUT";
  headers?: Record<string, string>;
}

// What an uploader declares about a blob before sending it. Both checksums
// cover the whole blob and are lowercase hex.
export interface Content {
  size_bytes: number;
  sha256: string;
  crc64nvme: string;
}

export interface MultipartUploadRequest {
  signPart: (partNumber: number, crc64nvme: string, signal?: AbortSignal) => Promise<UploadTarget>;
}

export type UploadAction =
  | { strategy: "direct-put"; target: UploadTarget }
  | { strategy: "multipart" };

export type UploadRequest =
  | Extract<UploadAction, { strategy: "direct-put" }>
  | {
      strategy: "multipart";
      multipart: MultipartUploadRequest;
    };

export interface UploadOptions {
  blob: Blob;
  signal?: AbortSignal;
  onProgress?: (progress: UploadProgress) => void;
}

// A blob is hashed one slice at a time. The slice bounds the memory held and
// how long a hashing step occupies the main thread; awaiting the next slice
// returns to the event loop. Smaller slices spend a growing share of the pass
// on reads.
const readSize = 512 * 1024;

// Bloby chooses 64 MiB as its browser base part size. S3 separately limits a
// multipart upload to 10,000 parts.
const multipartPartSize = 64 * 1024 * 1024;
const maximumMultipartParts = 10_000;

const partConcurrency = 4;
const partAttempts = 3;

// Reads the blob once, without holding it in memory. Progress is reported at
// each whole percent, not at each slice, which would be hundreds of times a
// second.
export async function digest(
  blob: Blob,
  options: { signal?: AbortSignal; onProgress?: (progress: UploadProgress) => void } = {},
): Promise<Content> {
  const sha = new SHA256();
  const crc = new Crc64Nvme();
  let loaded = 0;
  let reported = 0;
  for await (const bytes of read(blob, options.signal)) {
    sha.update(bytes);
    crc.update(bytes);
    loaded += bytes.length;
    const progress = uploadProgress(loaded, blob.size);
    if (progress.percent !== reported || loaded === blob.size) {
      reported = progress.percent;
      options.onProgress?.(progress);
    }
  }
  return {
    size_bytes: blob.size,
    sha256: uint8ArrayToHex(sha.digest()),
    crc64nvme: uint8ArrayToHex(await crc.digest()),
  };
}

// Resolves once storage has accepted every byte. Completing a multipart upload
// and finalizing the object remain server and application operations.
export async function upload(request: UploadRequest & UploadOptions): Promise<void> {
  const { blob, signal, onProgress } = request;
  const report = (loaded: number) => onProgress?.(uploadProgress(loaded, blob.size));
  if (request.strategy === "direct-put") {
    await put(request.target, blob, signal, report);
    return;
  }
  await uploadParts(request.multipart, blob, signal, report);
}

async function uploadParts(
  multipart: MultipartUploadRequest,
  blob: Blob,
  external: AbortSignal | undefined,
  report: (loaded: number) => void,
): Promise<void> {
  if (blob.size === 0) throw new Error("Multipart uploads require a nonempty blob.");

  const partSize = Math.max(multipartPartSize, Math.ceil(blob.size / maximumMultipartParts));
  const partCount = Math.ceil(blob.size / partSize);

  // The first part to fail aborts the others with its error.
  const failure = new AbortController();
  const signal = external ? AbortSignal.any([external, failure.signal]) : failure.signal;

  let loaded = 0;
  await pMap(
    Array.from({ length: partCount }, (_, index) => index),
    async (index) => {
      const part = blob.slice(index * partSize, (index + 1) * partSize);
      let partLoaded = 0;
      try {
        await uploadPart(multipart, index + 1, part, signal, (bytes) => {
          loaded += bytes - partLoaded;
          partLoaded = bytes;
          report(loaded);
        });
      } catch (error) {
        failure.abort(error);
        throw error;
      }
    },
    { concurrency: partConcurrency, signal },
  );
}

async function uploadPart(
  multipart: MultipartUploadRequest,
  partNumber: number,
  part: Blob,
  signal: AbortSignal,
  onLoaded: (loaded: number) => void,
): Promise<void> {
  const crc = new Crc64Nvme();
  for await (const bytes of read(part, signal)) crc.update(bytes);
  const checksum = uint8ArrayToHex(await crc.digest());

  // Every attempt asks for a fresh target; an earlier signature may have expired.
  await pRetry(
    async () => put(await multipart.signPart(partNumber, checksum, signal), part, signal, onLoaded),
    {
      retries: partAttempts - 1,
      signal,
      onFailedAttempt: () => {
        if (!signal.aborted) onLoaded(0);
      },
    },
  );
  onLoaded(part.size);
}

async function* read(blob: Blob, signal?: AbortSignal): AsyncGenerator<Uint8Array> {
  signal?.throwIfAborted();
  for (let offset = 0; offset < blob.size; offset += readSize) {
    const bytes = new Uint8Array(await blob.slice(offset, offset + readSize).arrayBuffer());
    signal?.throwIfAborted();
    yield bytes;
  }
}

function put(
  target: UploadTarget,
  body: Blob,
  signal: AbortSignal | undefined,
  onLoaded: (loaded: number) => void,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }

    const xhr = new XMLHttpRequest();
    const finish = () => signal?.removeEventListener("abort", abort);
    const abort = () => xhr.abort();

    xhr.upload.addEventListener("progress", (event) => onLoaded(event.loaded));
    xhr.addEventListener("load", () => {
      finish();
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve();
        return;
      }
      reject(new Error(`Upload failed with HTTP ${xhr.status}.`));
    });
    xhr.addEventListener("error", () => {
      finish();
      reject(new Error("Upload failed before the storage service accepted the request."));
    });
    xhr.addEventListener("abort", () => {
      finish();
      reject(signal?.reason ?? new DOMException("Upload aborted", "AbortError"));
    });

    signal?.addEventListener("abort", abort, { once: true });
    try {
      xhr.open(target.method, target.url);
      for (const [key, value] of Object.entries(target.headers ?? {})) {
        xhr.setRequestHeader(key, value);
      }
      xhr.send(body);
    } catch (error) {
      finish();
      reject(error);
    }
  });
}

function uploadProgress(loaded: number, total: number): UploadProgress {
  return {
    loaded,
    total,
    percent: total > 0 ? Math.round((loaded / total) * 100) : 0,
  };
}
