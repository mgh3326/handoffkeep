-- Run once as a PostgreSQL cluster administrator. Do not put the password in git:
-- psql -v handoffkeep_password='...' -f deploy/sql/handoffkeep.sql postgres
CREATE ROLE handoffkeep LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT
  PASSWORD :'handoffkeep_password';
CREATE DATABASE handoffkeep OWNER handoffkeep;
\connect handoffkeep
REVOKE ALL ON DATABASE handoffkeep FROM PUBLIC;
GRANT CONNECT ON DATABASE handoffkeep TO handoffkeep;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO handoffkeep;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
