#!/bin/sh
set -eu

serve_password=$(cat /run/secrets/postgres_serve_password)

psql --set=ON_ERROR_STOP=1 --set=serve_password="$serve_password" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON DATABASE dac FROM PUBLIC;

CREATE ROLE dac_serve LOGIN PASSWORD :'serve_password';
GRANT CONNECT ON DATABASE dac TO dac_serve;
GRANT USAGE ON SCHEMA public TO dac_serve;

ALTER DEFAULT PRIVILEGES FOR ROLE dac_migrate IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO dac_serve;
ALTER DEFAULT PRIVILEGES FOR ROLE dac_migrate IN SCHEMA public
  GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO dac_serve;
SQL
