#!/bin/sh
# Creates the two database roles of the service (#180) when the Docker
# Compose PostgreSQL starts with an empty data volume; the image runs every
# script of /docker-entrypoint-initdb.d once, as its superuser.
#
#   EXCHANGE_MIGRATIONS_USER  owns the database and its schema, and is used
#                             only by "admin migrate" (the migrate service).
#   EXCHANGE_RUNTIME_USER     is the role of the server. It owns nothing;
#                             "admin migrate" grants it its privileges.
#
# Production creates the same two roles with its own tooling; see
# docs/configuration.md#database-roles.
set -eu

: "${EXCHANGE_MIGRATIONS_USER:?}" "${EXCHANGE_MIGRATIONS_PASSWORD:?}"
: "${EXCHANGE_RUNTIME_USER:?}" "${EXCHANGE_RUNTIME_PASSWORD:?}"

# psql variables quote the names (:"name") and the passwords (:'name'), so
# no value is formatted into the SQL.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -v migrations_user="$EXCHANGE_MIGRATIONS_USER" -v migrations_password="$EXCHANGE_MIGRATIONS_PASSWORD" \
  -v runtime_user="$EXCHANGE_RUNTIME_USER" -v runtime_password="$EXCHANGE_RUNTIME_PASSWORD" \
  -v db="$POSTGRES_DB" <<'SQL'
CREATE ROLE :"migrations_user" LOGIN PASSWORD :'migrations_password';
CREATE ROLE :"runtime_user" LOGIN PASSWORD :'runtime_password';
-- The owner of the database owns its public schema too (PostgreSQL 15 and
-- later), and may create the extensions that the migrations need.
ALTER DATABASE :"db" OWNER TO :"migrations_user";
REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE :"db" TO :"runtime_user";
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
SQL
