-- +goose Up

ALTER TABLE storage_objects
    ADD COLUMN storage_key TEXT UNIQUE
        CHECK (storage_key IS NULL OR btrim(storage_key) <> '');

UPDATE storage_objects
SET storage_key = prefix || '/' || id || '/' || filename
WHERE available_at IS NOT NULL;

ALTER TABLE storage_objects DROP CONSTRAINT storage_objects_state_check;
ALTER TABLE storage_objects ADD CONSTRAINT storage_objects_state_check CHECK (
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
);
