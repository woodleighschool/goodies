import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";

import { upload } from "../src/index.js";

interface Response {
  status: number;
  etag?: string;
}

class FakeXMLHttpRequest {
  static responses: Response[] = [];
  static requests: FakeXMLHttpRequest[] = [];

  status = 0;
  method = "";
  url = "";
  body?: Blob;
  headers = new Map<string, string>();
  private listeners = new Map<string, (event: Event) => void>();
  private progress?: (event: ProgressEvent) => void;
  private etag: string | null = null;

  upload = {
    addEventListener: (_name: string, listener: (event: ProgressEvent) => void) => {
      this.progress = listener;
    },
  };

  constructor() {
    FakeXMLHttpRequest.requests.push(this);
  }

  addEventListener(name: string, listener: (event: Event) => void) {
    this.listeners.set(name, listener);
  }

  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }

  setRequestHeader(name: string, value: string) {
    this.headers.set(name, value);
  }

  getResponseHeader(name: string) {
    return name.toLowerCase() === "etag" ? this.etag : null;
  }

  send(body: Blob) {
    this.body = body;
    const response = FakeXMLHttpRequest.responses.shift() ?? { status: 204 };
    queueMicrotask(() => {
      this.progress?.({ loaded: body.size } as ProgressEvent);
      this.status = response.status;
      this.etag = response.etag ?? null;
      this.listeners.get("load")?.(new Event("load"));
    });
  }

  abort() {
    this.listeners.get("abort")?.(new Event("abort"));
  }
}

beforeEach(() => {
  FakeXMLHttpRequest.requests = [];
  FakeXMLHttpRequest.responses = [];
  globalThis.XMLHttpRequest = FakeXMLHttpRequest as unknown as typeof XMLHttpRequest;
});

afterEach(() => {
  Reflect.deleteProperty(globalThis, "XMLHttpRequest");
});

test("uploads a blob directly to the server-selected target", async () => {
  const blob = new Blob(["hello"]);
  const progress: number[] = [];

  const result = await upload({
    strategy: "direct-put",
    target: {
      url: "https://uploads.example/object",
      method: "PUT",
      headers: { "X-Test": "signed" },
    },
    blob,
    onProgress: (next) => progress.push(next.percent),
  });

  assert.deepEqual(result, { strategy: "direct-put" });
  assert.equal(FakeXMLHttpRequest.requests.length, 1);
  const request = FakeXMLHttpRequest.requests[0];
  assert.equal(request?.method, "PUT");
  assert.equal(request?.url, "https://uploads.example/object");
  assert.equal(request?.headers.get("X-Test"), "signed");
  assert.equal(request?.body, blob);
  assert.deepEqual(progress, [100]);
});

test("re-signs a failed multipart part and returns its ETag", async () => {
  FakeXMLHttpRequest.responses = [{ status: 503 }, { status: 200, etag: '"part-1"' }];
  const signed: number[] = [];

  const result = await upload({
    strategy: "multipart",
    multipart: {
      signPart: async (partNumber) => {
        signed.push(partNumber);
        return {
          url: `https://uploads.example/part/${signed.length}`,
          method: "PUT",
          headers: {},
        };
      },
    },
    blob: new Blob(["part"]),
  });

  assert.deepEqual(signed, [1, 1]);
  assert.deepEqual(result, {
    strategy: "multipart",
    parts: [{ part_number: 1, etag: '"part-1"' }],
  });
});

test("completes multipart uploads with ordered parts and aggregate progress", async () => {
  const partSize = 64 * 1024 * 1024;
  const blob = new Blob([new Uint8Array(partSize), "end"]);
  FakeXMLHttpRequest.responses = [
    { status: 200, etag: '"first"' },
    { status: 200, etag: '"last"' },
  ];
  const signed: number[] = [];
  const progress: number[] = [];
  const result = await upload({
    strategy: "multipart",
    multipart: {
      signPart: async (partNumber) => {
        signed.push(partNumber);
        return { url: `https://uploads.invalid/${partNumber}`, method: "PUT" };
      },
    },
    blob,
    onProgress: ({ loaded }) => progress.push(loaded),
  });

  assert.deepEqual(signed, [1, 2]);
  assert.deepEqual(
    FakeXMLHttpRequest.requests.map(({ body }) => body?.size),
    [partSize, 3],
  );
  assert.deepEqual(result, {
    strategy: "multipart",
    parts: [
      { part_number: 1, etag: '"first"' },
      { part_number: 2, etag: '"last"' },
    ],
  });
  assert.equal(progress.at(-1), blob.size);
  assert.ok(progress.every((loaded) => loaded <= blob.size));
});

test("does not transfer or sign parts after cancellation", async () => {
  const signal = AbortSignal.abort();
  const blob = new Blob(["cancelled"]);
  await assert.rejects(
    upload({
      strategy: "direct-put",
      target: { url: "https://uploads.invalid/object", method: "PUT" },
      signal,
      blob,
    }),
    /cancelled/,
  );
  await assert.rejects(
    upload({
      strategy: "multipart",
      multipart: {
        signPart: async () => {
          throw new Error("must not sign");
        },
      },
      signal,
      blob,
    }),
    /cancelled/,
  );
  assert.equal(FakeXMLHttpRequest.requests.length, 0);
});

test("rejects multipart completion when the provider never supplies an ETag", async () => {
  await assert.rejects(
    upload({
      strategy: "multipart",
      multipart: {
        signPart: async () => ({ url: "https://uploads.invalid/part", method: "PUT" }),
      },
      blob: new Blob(["part"]),
    }),
    /did not return an ETag/,
  );
  assert.equal(FakeXMLHttpRequest.requests.length, 2);
});
