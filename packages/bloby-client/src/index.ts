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

export interface MultipartUploadRequest {
  signPart: (partNumber: number, signal?: AbortSignal) => Promise<UploadTarget>;
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

export interface CompletedPart {
  part_number: number;
  etag: string;
}

export type UploadResult =
  | { strategy: "direct-put" }
  | { strategy: "multipart"; parts: CompletedPart[] };

type UploadContext = {
  blob: Blob;
  signal?: AbortSignal;
  onProgress?: (progress: UploadProgress) => void;
};

export type UploadExecution = UploadRequest & UploadContext;

// Bloby chooses 64 MiB as its browser base part size. S3 separately limits a
// multipart upload to 10,000 parts.
const multipartPartSize = 64 * 1024 * 1024;
const maximumMultipartParts = 10_000;

export async function upload(request: UploadExecution): Promise<UploadResult> {
  if (request.strategy === "direct-put") {
    await uploadTarget(
      request.target,
      request.blob,
      request.blob.size,
      0,
      request.onProgress,
      request.signal,
    );
    return { strategy: "direct-put" };
  }
  return uploadWithMultipartProgress(request);
}

async function uploadWithMultipartProgress(
  request: Extract<UploadExecution, { strategy: "multipart" }>,
): Promise<UploadResult> {
  const { blob, signal, onProgress } = request;
  throwIfCancelled(signal);

  const partSize = Math.max(multipartPartSize, Math.ceil(blob.size / maximumMultipartParts));
  const parts: CompletedPart[] = [];
  let completedBytes = 0;

  for (let offset = 0, partNumber = 1; offset < blob.size; offset += partSize, partNumber++) {
    const chunk = blob.slice(offset, Math.min(offset + partSize, blob.size));
    const etag = await uploadPart(
      request.multipart,
      partNumber,
      chunk,
      blob.size,
      completedBytes,
      onProgress,
      signal,
    );
    parts.push({ part_number: partNumber, etag });
    completedBytes += chunk.size;
    onProgress?.(uploadProgress(completedBytes, blob.size));
  }

  return { strategy: "multipart", parts };
}

async function uploadPart(
  multipart: MultipartUploadRequest,
  partNumber: number,
  chunk: Blob,
  totalBytes: number,
  completedBytes: number,
  onProgress?: (progress: UploadProgress) => void,
  signal?: AbortSignal,
) {
  let lastError: unknown;
  for (let attempt = 0; attempt < 2; attempt++) {
    throwIfCancelled(signal);
    const target = await multipart.signPart(partNumber, signal);
    try {
      const etag = await uploadTarget(
        target,
        chunk,
        totalBytes,
        completedBytes,
        onProgress,
        signal,
      );
      if (!etag) {
        throw new Error(`Multipart part ${partNumber} did not return an ETag.`);
      }
      return etag;
    } catch (error) {
      if (signal?.aborted) throw cancelledError();
      lastError = error;
      onProgress?.(uploadProgress(completedBytes, totalBytes));
    }
  }
  throw lastError;
}

function uploadTarget(
  target: UploadTarget,
  body: Blob,
  totalBytes: number,
  completedBytes: number,
  onProgress?: (progress: UploadProgress) => void,
  signal?: AbortSignal,
) {
  return new Promise<string | null>((resolve, reject) => {
    if (signal?.aborted) {
      reject(cancelledError());
      return;
    }

    const xhr = new XMLHttpRequest();
    const finish = () => signal?.removeEventListener("abort", abort);
    const abort = () => xhr.abort();

    xhr.upload.addEventListener("progress", (event) => {
      onProgress?.(uploadProgress(completedBytes + event.loaded, totalBytes));
    });
    xhr.addEventListener("load", () => {
      finish();
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(xhr.getResponseHeader("ETag"));
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
      reject(cancelledError());
    });

    signal?.addEventListener("abort", abort, { once: true });
    xhr.open(target.method, target.url);
    for (const [key, value] of Object.entries(target.headers ?? {})) {
      xhr.setRequestHeader(key, value);
    }
    xhr.send(body);
  });
}

function uploadProgress(loaded: number, total: number): UploadProgress {
  return {
    loaded,
    total,
    percent: total > 0 ? Math.round((loaded / total) * 100) : 0,
  };
}

function throwIfCancelled(signal?: AbortSignal) {
  if (signal?.aborted) throw cancelledError();
}

function cancelledError() {
  return new Error("Upload cancelled.");
}
