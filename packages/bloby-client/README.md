# @woodleighschool/bloby-client

Headless browser transfers for [Bloby](../../bloby). `digest` produces what an application declares when it creates an upload. The server chooses direct or multipart ingestion. `upload` resolves once storage has accepted every byte; applications own creating and finalizing the upload, attachment and UI.

```ts
const content = await digest(file, { signal, onProgress });
// { size_bytes, sha256, crc64nvme }, declared when creating the upload.

await upload({
  strategy: "multipart",
  blob: file,
  signal,
  onProgress,
  multipart: {
    signPart: (partNumber, crc64nvme, signal) => signPartRequest(partNumber, crc64nvme, signal),
  },
});
```

`digest` reads the blob once, in bounded slices, without holding it in memory. It returns the size with the SHA-256 and CRC-64/NVME of the whole blob as lowercase hex, and reports progress at each whole percent. It runs on the main thread and returns to the event loop between slices.

For a direct upload, pass the server's `{ strategy: "direct-put", target }` action together with the blob and options. The client sends `target.headers` as given; storage accepts only the declared bytes.

A multipart upload sends 64 MiB parts, four at a time; parts grow when a blob would otherwise need more than 10,000. For each part the client computes its CRC-64/NVME, asks `signPart` for a target signed for that checksum, and sends the part with the returned headers. A failed part is attempted three times in total, each with a fresh target, after a short backoff. The first part to exhaust its attempts stops the others and rejects `upload` with its error. Progress is the sum of finished parts and bytes in flight, so it steps back when a part is retried.

`signPart` adapts an application endpoint. Cancellation preserves the supplied `AbortSignal` reason and stops further signing and transfers. Completing the multipart upload and finalizing or attaching the object are server and application operations.

The package has no React, router or API-client dependency, and uses no WebAssembly, workers or `eval`, so it runs under `script-src 'self'`.
