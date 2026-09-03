package bloby

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestS3ProviderImmutableLifecycle(t *testing.T) {
	endpoint := os.Getenv("BLOBY_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("BLOBY_TEST_S3_ENDPOINT is not set")
	}
	service, err := New(t.Context(), newMemoryRegistry(), Config{Kind: KindS3, TransferTTL: time.Minute, S3: S3Config{
		Endpoint: endpoint, Region: os.Getenv("BLOBY_TEST_S3_REGION"), Bucket: os.Getenv("BLOBY_TEST_S3_BUCKET"), AccessKey: os.Getenv("BLOBY_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("BLOBY_TEST_S3_SECRET_KEY"), PathStyle: true,
	}}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("integration/%d", time.Now().UnixNano())
	t.Run("direct replay and concurrent finalization", func(t *testing.T) {
		object, action, err := service.BeginDirect(t.Context(), prefix, "report.txt")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { service.DeleteUnreferenced(t.Context(), object.ID) })
		uploadS3Target(t, *action.Target, "original direct bytes")
		var group sync.WaitGroup
		for range 4 {
			group.Go(func() {
				if _, err := service.Finalize(t.Context(), object.ID, prefix); err != nil {
					t.Errorf("finalize: %v", err)
				}
			})
		}
		group.Wait()
		uploadS3Target(t, *action.Target, "replayed direct bytes")
		available, err := service.Finalize(t.Context(), object.ID, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAvailable(t, service, *available); got != "original direct bytes" {
			t.Fatalf("published replay %q", got)
		}
	})
	t.Run("multipart replay and finalize retry", func(t *testing.T) {
		object, _, err := service.Begin(t.Context(), prefix, "multipart.txt", s3MultipartThreshold+1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { service.DeleteUnreferenced(t.Context(), object.ID) })
		bodies := []string{strings.Repeat("a", 5*1024*1024), "final part"}
		parts := make([]CompletedPart, len(bodies))
		for i, body := range bodies {
			partNumber := int32(i + 1)
			target, err := service.PresignMultipartPart(t.Context(), object.ID, prefix, partNumber)
			if err != nil {
				t.Fatal(err)
			}
			parts[i] = CompletedPart{PartNumber: partNumber, ETag: uploadS3Target(t, target, body)}
		}
		if err := service.CompleteMultipart(t.Context(), object.ID, prefix, parts); err != nil {
			t.Fatal(err)
		}
		available, err := service.Finalize(t.Context(), object.ID, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.CompleteMultipart(t.Context(), object.ID, prefix, parts); err != nil {
			t.Fatalf("completion retry: %v", err)
		}
		if got := readAvailable(t, service, *available); got != strings.Join(bodies, "") {
			t.Fatal("multipart published bytes differ")
		}
	})
	t.Run("competing snapshots on provider without conditional writes", func(t *testing.T) {
		testCompetingFinalizers(t, service)
	})
}
