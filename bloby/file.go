package bloby

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// fileStore keeps blobs under a local directory. Keys map to paths beneath root.
type fileStore struct {
	root           string
	baseURL        string
	transferOrigin string
	capabilityKey  []byte
	ttl            time.Duration
}

func (s *fileStore) beginUpload(ctx context.Context, key string, content Content) (UploadAction, error) {
	target, err := s.PresignPut(ctx, key, content, 0)
	if err != nil {
		return UploadAction{}, err
	}
	return UploadAction{Strategy: StrategyDirectPut, Target: &target}, nil
}

func newFileStore(root, baseURL, capabilityKeyHex string, ttl time.Duration) (*fileStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("storage file root is empty")
	}
	capabilityKey, err := hex.DecodeString(capabilityKeyHex)
	if err != nil || len(capabilityKey) != 32 {
		return nil, errors.New("storage capability key must encode exactly 32 bytes as hexadecimal")
	}
	origin, err := transferOrigin(baseURL)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage file root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create storage file root: %w", err)
	}
	return &fileStore{
		root:           abs,
		baseURL:        baseURL,
		transferOrigin: origin,
		capabilityKey:  slices.Clone(capabilityKey),
		ttl:            ttl,
	}, nil
}

func (s *fileStore) TransferOrigin() string {
	return s.transferOrigin
}

// resolve maps a storage key to a path under root, rejecting traversal.
func (s *fileStore) resolve(key string) (string, error) {
	if slices.Contains(strings.Split(key, "/"), "..") {
		return "", fmt.Errorf("invalid storage key %q", key)
	}
	path := filepath.Join(s.root, filepath.FromSlash(key))
	if path != s.root && !strings.HasPrefix(path, s.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid storage key %q", key)
	}
	return path, nil
}

func (s *fileStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	file, err := s.openFile(key)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (s *fileStore) openFile(key string) (*os.File, error) {
	path, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // resolve confines the path to the configured storage root.
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", key, err)
	}
	return f, nil
}

// Put writes beside the destination and renames into place only once the body
// matched content, so a key never holds anything but its declared bytes.
func (s *fileStore) Put(_ context.Context, key string, r io.Reader, content Content, _ putOptions) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	// #nosec G703 -- path comes from resolve, which rejects traversal and
	// constrains keys to the configured storage root.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create dir for %q: %w", key, err)
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("create temp for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	sum := sha256.New()
	// One byte past the declaration exposes an oversized body without storing it.
	written, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(r, content.SizeBytes+1))
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %q: %w", key, err)
	}
	if written != content.SizeBytes || hex.EncodeToString(sum.Sum(nil)) != content.SHA256 {
		return contentMismatch("body is not the declared %d bytes", content.SizeBytes)
	}
	// #nosec G703 -- tmpName is created under the already-resolved storage dir.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod %q: %w", key, err)
	}
	// #nosec G703 -- path comes from resolve, which keeps writes under root.
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit %q: %w", key, err)
	}
	return nil
}

func (s *fileStore) Delete(_ context.Context, key string) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	// Prune the now-empty parent directory; ignore if it has siblings.
	_ = os.Remove(filepath.Dir(path))
	return nil
}

func (s *fileStore) PresignGet(
	_ context.Context,
	key string,
	ttl time.Duration,
	opts getOptions,
) (string, error) {
	return s.blobURL(capabilityClaims{
		Op:          capabilityGet,
		Key:         key,
		Exp:         time.Now().Add(s.expires(ttl)).Unix(),
		ContentType: opts.ContentType,
	})
}

func (s *fileStore) PresignPut(
	_ context.Context,
	key string,
	content Content,
	ttl time.Duration,
) (UploadTarget, error) {
	url, err := s.blobURL(capabilityClaims{
		Op:        capabilityPut,
		Key:       key,
		Exp:       time.Now().Add(s.expires(ttl)).Unix(),
		SizeBytes: content.SizeBytes,
		SHA256:    content.SHA256,
	})
	if err != nil {
		return UploadTarget{}, err
	}
	return UploadTarget{
		URL:    url,
		Method: http.MethodPut,
	}, nil
}

func (s *fileStore) blobURL(claims capabilityClaims) (string, error) {
	token := signCapability(s.capabilityKey, claims)
	blobURL, err := url.Parse(strings.TrimRight(s.baseURL, "/") + "/storage/" + escapePath(claims.Key))
	if err != nil {
		return "", err
	}
	values := blobURL.Query()
	values.Set("cap", token)
	blobURL.RawQuery = values.Encode()
	return blobURL.String(), nil
}

func escapePath(value string) string {
	parts := strings.Split(value, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func (s *fileStore) expires(ttl time.Duration) time.Duration {
	return ttlOrDefault(ttl, s.ttl)
}

// verify checks the stored size. Put admits only bytes with the declared
// SHA-256, so a file of the right size is the declared content.
func (s *fileStore) verify(_ context.Context, key string, content Content) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrObjectNotFound
	}
	if err != nil {
		return fmt.Errorf("stat %q: %w", key, err)
	}
	if info.Size() != content.SizeBytes {
		return contentMismatch("storage holds %d bytes, declared %d", info.Size(), content.SizeBytes)
	}
	return nil
}

func (s *fileStore) head(_ context.Context, key string, n int64) ([]byte, error) {
	file, err := s.openFile(key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	head, err := io.ReadAll(io.LimitReader(file, n))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", key, err)
	}
	return head, nil
}

func (s *fileStore) expiredObjects(ctx context.Context, before time.Time) ([]string, error) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var keys []string
	err = fs.WalkDir(root.FS(), strings.TrimSuffix(objectPrefix, "/"), func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.ModTime().Before(before) {
			keys = append(keys, path)
		}
		return nil
	})
	return keys, err
}
