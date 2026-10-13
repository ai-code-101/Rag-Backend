-- Email verification and one-time codes were removed: accounts are usable as soon as
-- they sign up. Existing databases drop the OTP table and the verification flag.
DROP TABLE IF EXISTS email_otps;
ALTER TABLE users DROP COLUMN IF EXISTS email_verified;
