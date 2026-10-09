import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { afterEach, beforeEach, mock, test } from "node:test";

import { Crc64Nvme } from "@aws-sdk/crc64-nvme";

import { digest, upload, type UploadProgress, type UploadTarget } from "../src/index.js";

// Records each request and holds it open until the test answers it.
class FakeXMLHttpRequest {
  static sent: FakeXMLHttpRequest[] = [];

  status = 0;
  method = "";
  url = "";
  body = new Blob();
  headers = new Map<string, string>();
  state: "unsent" | "in-flight" | "done" | "aborted" = "unsent";
  private listeners = new Map<string, () => void>();
  private uploadProgress?: (event: ProgressEvent) => void;

  upload = {
    addEventListener: (_name: string, listener: (event: ProgressEvent) => void) => {
      this.uploadProgress = listener;
    },
  };

  addEventListener(name: string, listener: () => void) {
    this.listeners.set(name, listener);
  }

  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }

  setRequestHeader(name: string, value: string) {
    this.headers.set(name, value);
  }

  send(body: Blob) {
    this.body = body;
    this.state = "in-flight";
    FakeXMLHttpRequest.sent.push(this);
  }

  abort() {
    if (this.state !== "in-flight") return;
    this.state = "aborted";
    this.listeners.get("abort")?.();
  }

  progress(loaded: number) {
    this.uploadProgress?.({ loaded } as ProgressEvent);
  }

  respond(status: number) {
    this.status = status;
    this.state = "done";
    this.listeners.get("load")?.();
  }

  loseConnection() {
    this.state = "done";
    this.listeners.get("error")?.();
  }
}

const inFlight = () => FakeXMLHttpRequest.sent.filter((request) => request.state === "in-flight");

// The part a request carries, read from the URLs partSigner issues.
const partOf = (request: FakeXMLHttpRequest) => Number(new URL(request.url).pathname.slice(1));

const partInFlight = (partNumber: number) =>
  inFlight().find((request) => partOf(request) === partNumber);

const turn = () => new Promise<void>((resolve) => setImmediate(resolve));

// Blob reads settle over many event-loop turns, so tests wait for what they
// observe instead of counting turns.
async function until(condition: () => boolean, description: string) {
  const deadline = performance.now() + 10_000;
  while (!condition()) {
    if (performance.now() > deadline) assert.fail(`Timed out waiting for ${description}.`);
    await turn();
  }
}

// A failed part waits on a timer before its next attempt.
async function elapseRetryDelay() {
  await turn();
  mock.timers.runAll();
  await turn();
}

// Stands in for the application's part-signing endpoint. The checksum header
// is base64, as S3 expects it.
function partSigner() {
  const calls: { partNumber: number; crc64nvme: string }[] = [];
  const signPart = async (partNumber: number, crc64nvme: string): Promise<UploadTarget> => {
    calls.push({ partNumber, crc64nvme });
    return {
      url: `https://uploads.invalid/${partNumber}?signature=${calls.length}`,
      method: "PUT",
      headers: { "x-amz-checksum-crc64nvme": Buffer.from(crc64nvme, "hex").toString("base64") },
    };
  };
  return { calls, signPart };
}

async function crc64nvme(...chunks: Uint8Array[]) {
  const crc = new Crc64Nvme();
  for (const chunk of chunks) crc.update(chunk);
  return Buffer.from(await crc.digest()).toString("hex");
}

// Bit-at-a-time CRC-64/NVME, sharing nothing with the table-driven dependency.
function referenceCrc64nvme(bytes: Uint8Array) {
  let crc = 0xffff_ffff_ffff_ffffn;
  for (const byte of bytes) {
    crc ^= BigInt(byte);
    for (let bit = 0; bit < 8; bit++) {
      crc = crc & 1n ? (crc >> 1n) ^ 0x9a6c_9329_ac4b_c9b5n : crc >> 1n;
    }
  }
  return (crc ^ 0xffff_ffff_ffff_ffffn).toString(16).padStart(16, "0");
}

function pseudoRandomBytes(length: number) {
  const words = new Uint32Array(Math.ceil(length / 4));
  let word = 0x9e37_79b9;
  for (let i = 0; i < words.length; i++) {
    word ^= word << 13;
    word ^= word >>> 17;
    word ^= word << 5;
    words[i] = word;
  }
  return new Uint8Array(words.buffer, 0, length);
}

const ascii = (text: string) => new TextEncoder().encode(text);

// Four full parts and a short fifth. Each full part opens with its own number,
// so no two parts share a checksum.
const partSize = 64 * 1024 * 1024;
const fiveParts = (() => {
  const filler = pseudoRandomBytes(partSize).subarray(1);
  return new Blob([...[1, 2, 3, 4].flatMap((part) => [Uint8Array.of(part), filler]), "end"]);
})();
const twoParts = fiveParts.slice(3 * partSize);

beforeEach(() => {
  FakeXMLHttpRequest.sent = [];
  globalThis.XMLHttpRequest = FakeXMLHttpRequest as unknown as typeof XMLHttpRequest;
  mock.timers.enable({ apis: ["setTimeout"] });
});

afterEach(() => {
  mock.timers.reset();
  Reflect.deleteProperty(globalThis, "XMLHttpRequest");
});

test("digest declares the size and checksums of a blob", async () => {
  assert.deepEqual(await digest(new Blob([])), {
    size_bytes: 0,
    sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    crc64nvme: "0000000000000000",
  });
  assert.deepEqual(await digest(new Blob(["123456789"])), {
    size_bytes: 9,
    sha256: "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225",
    crc64nvme: "ae8b14860a799888",
  });
});

test("digest covers a blob that takes several reads", async () => {
  const bytes = pseudoRandomBytes(2 * 1024 * 1024 + 4099);

  assert.deepEqual(await digest(new Blob([bytes])), {
    size_bytes: bytes.length,
    sha256: createHash("sha256").update(bytes).digest("hex"),
    crc64nvme: referenceCrc64nvme(bytes),
  });
});

test("digest stays correct once the SHA-256 bit length passes 32 bits", async () => {
  const blob = new Blob([fiveParts, fiveParts]);
  assert.ok(blob.size * 8 > 2 ** 32);

  const sha = createHash("sha256");
  for await (const chunk of blob.stream()) sha.update(chunk);
  assert.equal((await digest(blob)).sha256, sha.digest("hex"));
});

test("digest reports each whole percent up to the total", async () => {
  const reports: UploadProgress[] = [];
  await digest(twoParts, { onProgress: (progress) => reports.push(progress) });

  const last = reports.pop();
  assert.deepEqual(last, { loaded: twoParts.size, total: twoParts.size, percent: 100 });
  assert.ok(reports.length > 50);
  const percents = reports.map((progress) => progress.percent);
  assert.deepEqual(
    percents,
    [...new Set(percents)].sort((a, b) => a - b),
  );
  assert.ok(reports.every((progress) => progress.loaded < twoParts.size));
});

test("digest rejects with the caller's reason when cancelled", async () => {
  const blob = new Blob([pseudoRandomBytes(2 * 1024 * 1024 + 4099)]);
  const reason = new Error("cancel this digest");

  let reports = 0;
  await assert.rejects(
    digest(blob, { signal: AbortSignal.abort(reason), onProgress: () => reports++ }),
    (error) => error === reason,
  );
  assert.equal(reports, 0);

  const controller = new AbortController();
  await assert.rejects(
    digest(blob, {
      signal: controller.signal,
      onProgress: () => {
        reports++;
        controller.abort(reason);
      },
    }),
    (error) => error === reason,
  );
  assert.equal(reports, 1);
});

test("uploads a blob directly to the server-selected target", async () => {
  const blob = new Blob(["hello"]);
  const progress: number[] = [];

  const done = upload({
    strategy: "direct-put",
    target: {
      url: "https://uploads.invalid/object",
      method: "PUT",
      headers: { "x-amz-content-sha256": "signed" },
    },
    blob,
    onProgress: (next) => progress.push(next.percent),
  });

  assert.equal(FakeXMLHttpRequest.sent.length, 1);
  const request = FakeXMLHttpRequest.sent[0]!;
  assert.equal(request.method, "PUT");
  assert.equal(request.url, "https://uploads.invalid/object");
  assert.deepEqual([...request.headers], [["x-amz-content-sha256", "signed"]]);
  assert.equal(request.body, blob);

  request.progress(blob.size);
  request.respond(200);
  await done;
  assert.deepEqual(progress, [100]);
});

test("rejects a direct upload that storage refuses", async () => {
  const rejected = assert.rejects(
    upload({
      strategy: "direct-put",
      target: { url: "https://uploads.invalid/object", method: "PUT" },
      blob: new Blob(["hello"]),
    }),
    /HTTP 400/,
  );
  FakeXMLHttpRequest.sent[0]!.respond(400);
  await rejected;
  assert.equal(FakeXMLHttpRequest.sent.length, 1);
});

test("sends every part with at most four in flight", async () => {
  const signer = partSigner();
  const done = upload({ strategy: "multipart", multipart: signer, blob: fiveParts });

  // The short fifth part would be first to reach storage if it were started
  // alongside the others.
  await until(() => inFlight().length >= 4, "four parts in flight");
  assert.deepEqual(inFlight().map(partOf).sort(), [1, 2, 3, 4]);
  assert.equal(signer.calls.length, 4);

  partInFlight(1)!.respond(200);
  await until(() => inFlight().length >= 4, "the fifth part to take the free slot");
  assert.deepEqual(inFlight().map(partOf).sort(), [2, 3, 4, 5]);

  for (const request of inFlight()) request.respond(200);
  await done;

  assert.deepEqual(
    FakeXMLHttpRequest.sent
      .map((request) => [partOf(request), request.body.size])
      .sort(([a], [b]) => a! - b!),
    [
      [1, partSize],
      [2, partSize],
      [3, partSize],
      [4, partSize],
      [5, 3],
    ],
  );
});

test("signs each part with the checksum of its own bytes", async () => {
  const signer = partSigner();
  const done = upload({ strategy: "multipart", multipart: signer, blob: twoParts });

  await until(() => inFlight().length === 2, "both parts in flight");
  const parts = await Promise.all(
    [twoParts.slice(0, partSize), twoParts.slice(partSize)].map(async (part) =>
      crc64nvme(new Uint8Array(await part.arrayBuffer())),
    ),
  );
  assert.notEqual(parts[0], parts[1]);
  assert.deepEqual(
    [...signer.calls].sort((a, b) => a.partNumber - b.partNumber),
    [
      { partNumber: 1, crc64nvme: parts[0] },
      { partNumber: 2, crc64nvme: parts[1] },
    ],
  );
  for (const [index, checksum] of parts.entries()) {
    const request = partInFlight(index + 1)!;
    assert.equal(await crc64nvme(new Uint8Array(await request.body.arrayBuffer())), checksum);
    assert.equal(
      request.headers.get("x-amz-checksum-crc64nvme"),
      Buffer.from(checksum!, "hex").toString("base64"),
    );
  }

  for (const request of inFlight()) request.respond(200);
  await done;
});

test("reports progress as the sum of finished and in-flight parts", async () => {
  const loaded: number[] = [];
  const done = upload({
    strategy: "multipart",
    multipart: partSigner(),
    blob: twoParts,
    onProgress: (progress) => loaded.push(progress.loaded),
  });

  await until(() => inFlight().length === 2, "both parts in flight");
  partInFlight(1)!.progress(1000);
  partInFlight(2)!.progress(2);
  partInFlight(1)!.progress(5000);
  assert.deepEqual(loaded, [1000, 1002, 5002]);

  partInFlight(1)!.respond(200);
  await turn();
  assert.equal(loaded.at(-1), partSize + 2);
  partInFlight(2)!.respond(200);
  await done;
  assert.equal(loaded.at(-1), twoParts.size);
});

test("retries a failed part with a fresh signature", async () => {
  const signer = partSigner();
  const loaded: number[] = [];
  const done = upload({
    strategy: "multipart",
    multipart: signer,
    blob: new Blob(["part"]),
    onProgress: (progress) => loaded.push(progress.loaded),
  });

  await until(() => inFlight().length === 1, "the first attempt");
  const first = inFlight()[0]!;
  first.progress(3);
  first.respond(503);
  await turn();
  assert.equal(signer.calls.length, 1, "the retry waits before signing again");
  assert.equal(loaded.at(-1), 0);

  await elapseRetryDelay();
  await until(() => inFlight().length === 1, "the second attempt");
  const second = inFlight()[0]!;
  assert.notEqual(second.url, first.url);
  assert.equal(second.body.size, 4);
  const checksum = await crc64nvme(ascii("part"));
  assert.deepEqual(signer.calls, [
    { partNumber: 1, crc64nvme: checksum },
    { partNumber: 1, crc64nvme: checksum },
  ]);

  second.respond(200);
  await done;
  assert.equal(loaded.at(-1), 4);
});

test("retries a part whose signing fails", async () => {
  const signer = partSigner();
  let failures = 1;
  const done = upload({
    strategy: "multipart",
    multipart: {
      signPart: async (partNumber, checksum) => {
        if (failures-- > 0) throw new Error("signing endpoint unavailable");
        return signer.signPart(partNumber, checksum);
      },
    },
    blob: new Blob(["part"]),
  });

  await elapseRetryDelay();
  await until(() => inFlight().length === 1, "the attempt after signing recovers");
  inFlight()[0]!.respond(200);
  await done;
  assert.equal(FakeXMLHttpRequest.sent.length, 1);
});

test("stops sibling parts once a part exhausts its attempts", async () => {
  const signer = partSigner();
  const rejected = assert.rejects(
    upload({ strategy: "multipart", multipart: signer, blob: twoParts }),
    /HTTP 503/,
  );

  await until(() => inFlight().length === 2, "both parts in flight");
  const sibling = partInFlight(1)!;
  const failures = [
    (attempt: FakeXMLHttpRequest) => attempt.respond(500),
    (attempt: FakeXMLHttpRequest) => attempt.loseConnection(),
    (attempt: FakeXMLHttpRequest) => attempt.respond(503),
  ];
  for (const fail of failures) {
    await until(() => partInFlight(2) !== undefined, "an attempt at the failing part");
    fail(partInFlight(2)!);
    await elapseRetryDelay();
  }
  await rejected;

  assert.equal(sibling.state, "aborted");
  await elapseRetryDelay();
  assert.deepEqual(signer.calls.map((call) => call.partNumber).sort(), [1, 2, 2, 2]);
  assert.equal(FakeXMLHttpRequest.sent.length, 4);
});

test("does not sign or transfer once cancelled", async () => {
  const reason = new Error("cancel this upload");
  const signal = AbortSignal.abort(reason);
  const blob = new Blob(["cancelled"]);
  const signer = partSigner();

  await assert.rejects(
    upload({
      strategy: "direct-put",
      target: { url: "https://uploads.invalid/object", method: "PUT" },
      signal,
      blob,
    }),
    (error) => error === reason,
  );
  await assert.rejects(
    upload({ strategy: "multipart", multipart: signer, signal, blob }),
    (error) => error === reason,
  );
  assert.equal(signer.calls.length, 0);
  assert.equal(FakeXMLHttpRequest.sent.length, 0);
});

test("cancellation during signing stops before sending bytes", async () => {
  const controller = new AbortController();
  const reason = new Error("cancel this upload");
  let signed = 0;

  await assert.rejects(
    upload({
      strategy: "multipart",
      blob: new Blob(["part"]),
      signal: controller.signal,
      multipart: {
        signPart: async (partNumber) => {
          signed++;
          controller.abort(reason);
          return { url: `https://uploads.invalid/${partNumber}`, method: "PUT" };
        },
      },
    }),
    (error) => error === reason,
  );
  assert.equal(signed, 1);
  assert.equal(FakeXMLHttpRequest.sent.length, 0);
});

test("cancellation aborts a direct upload in flight", async () => {
  const controller = new AbortController();
  const reason = new Error("cancel this upload");
  const rejected = assert.rejects(
    upload({
      strategy: "direct-put",
      target: { url: "https://uploads.invalid/object", method: "PUT" },
      blob: new Blob(["hello"]),
      signal: controller.signal,
    }),
    (error) => error === reason,
  );

  controller.abort(reason);
  await rejected;
  assert.equal(FakeXMLHttpRequest.sent[0]!.state, "aborted");
});

test("cancellation during transfer aborts the parts in flight", async () => {
  const controller = new AbortController();
  // A reason need not be an Error.
  const reason = "left the page";
  const signer = partSigner();
  const rejected = assert.rejects(
    upload({ strategy: "multipart", multipart: signer, blob: twoParts, signal: controller.signal }),
    (error) => error === reason,
  );

  await until(() => inFlight().length === 2, "both parts in flight");
  controller.abort(reason);
  await rejected;

  assert.deepEqual(
    FakeXMLHttpRequest.sent.map((request) => request.state),
    ["aborted", "aborted"],
  );
  await elapseRetryDelay();
  assert.equal(signer.calls.length, 2);
});
