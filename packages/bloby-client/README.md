# @woodleighschool/bloby-client

Headless browser transfers for [Bloby](../../bloby). The server chooses direct or multipart ingestion. `upload` resolves after the complete transfer is accepted by storage; applications own publication, attachment and UI.

```ts
await upload({
  strategy: "multipart",
  blob: file,
  signal,
  onProgress,
  multipart: {
    signPart: (partNumber, signal) => signPartRequest(partNumber, signal),
    complete: (parts, signal) => completeMultipartRequest(parts, signal),
  },
});
```

The callbacks adapt application endpoints. Multipart completion must be idempotent: the client retries a failed completion once without retransmitting the parts. Part transfers also retry once with a fresh signed target. Cancellation preserves the supplied `AbortSignal` reason and stops further signing, transfers and completion retries. Finalization or attachment of the resulting object is a separate application operation.

For a direct upload, pass the server's `{ strategy: "direct-put", target }` action together with the blob and options. The core package has no React, router or API-client dependency.
