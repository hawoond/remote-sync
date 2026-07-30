ALTER TABLE folders
    ADD COLUMN display_name text NOT NULL DEFAULT ''
        CHECK (char_length(display_name) <= 128),
    ADD COLUMN client_key text
        CHECK (client_key IS NULL OR char_length(client_key) BETWEEN 1 AND 128);

CREATE UNIQUE INDEX folders_owner_client_key_idx
    ON folders (owner_user_id, client_key)
    WHERE client_key IS NOT NULL;
