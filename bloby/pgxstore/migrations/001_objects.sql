-- +goose Up

CREATE TABLE storage_objects (
    id BIGSERIAL PRIMARY KEY,
    prefix TEXT NOT NULL,
    filename TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    size_bytes BIGINT CHECK (size_bytes IS NULL OR size_bytes >= 0),
    sha256 TEXT CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),
    multipart_upload_id TEXT CHECK (multipart_upload_id IS NULL OR btrim(multipart_upload_id) <> ''),
    storage_key TEXT UNIQUE CHECK (storage_key IS NULL OR btrim(storage_key) <> ''),
    available_at TIMESTAMPTZ,
    expired_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT storage_objects_state_check CHECK (
        (
            available_at IS NULL
            AND content_type = ''
            AND size_bytes IS NULL
            AND sha256 IS NULL
            AND storage_key IS NULL
        )
        OR (
            available_at IS NOT NULL
            AND content_type <> ''
            AND size_bytes IS NOT NULL
            AND sha256 IS NOT NULL
            AND storage_key IS NOT NULL
            AND multipart_upload_id IS NULL
        )
    ),
    CONSTRAINT storage_objects_expiry_check CHECK (
        expired_at IS NULL OR available_at IS NULL
    )
);

CREATE INDEX storage_objects_prefix_idx
    ON storage_objects (prefix);
CREATE INDEX storage_objects_pending_expiry_idx
    ON storage_objects (updated_at, id)
    WHERE available_at IS NULL;
