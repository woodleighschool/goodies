-- +goose Up

ALTER TABLE storage_objects
    ADD COLUMN crc64nvme TEXT CHECK (crc64nvme IS NULL OR crc64nvme ~ '^[0-9a-f]{16}$');

-- Uploads begun without a declaration cannot be published.
UPDATE storage_objects
SET expired_at = now()
WHERE available_at IS NULL AND expired_at IS NULL;

ALTER TABLE storage_objects DROP CONSTRAINT storage_objects_state_check;
ALTER TABLE storage_objects ADD CONSTRAINT storage_objects_state_check CHECK (
    (
        available_at IS NULL
        AND content_type = ''
        AND storage_key IS NULL
        AND (
            expired_at IS NOT NULL
            OR (size_bytes IS NOT NULL AND sha256 IS NOT NULL AND crc64nvme IS NOT NULL)
        )
    )
    OR (
        available_at IS NOT NULL
        AND content_type <> ''
        AND size_bytes IS NOT NULL
        AND sha256 IS NOT NULL
        AND storage_key IS NOT NULL
        AND multipart_upload_id IS NULL
    )
);
