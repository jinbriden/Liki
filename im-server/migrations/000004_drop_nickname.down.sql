ALTER TABLE users ADD COLUMN nickname VARCHAR(64) NOT NULL DEFAULT '';
UPDATE users SET nickname = username;
