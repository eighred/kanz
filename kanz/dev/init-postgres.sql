-- DEV-ONLY: public fixture credentials, never production configuration.
-- Keep destructive test fixtures separate from the running smoke service.
CREATE ROLE kanzapp LOGIN PASSWORD 'kanz' NOSUPERUSER NOBYPASSRLS;
CREATE DATABASE kanzapp OWNER kanzapp;
CREATE DATABASE kanz_risk_dev OWNER kanzapp;
