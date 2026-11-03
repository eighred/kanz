-- A scheduled parent's original approved debit remains the allocation ceiling
-- even if a later execution exceeds it and raises required_debit to preserve
-- the actual liability. Existing commitments have NULL admission_debit and
-- cannot allocate new children until explicitly reconciled.
ALTER TABLE capital_commitments ADD COLUMN admission_debit text;
ALTER TABLE capital_commitments ADD COLUMN allocated_debit text NOT NULL DEFAULT '0';
ALTER TABLE capital_members ADD COLUMN allocated_debit text NOT NULL DEFAULT '0';
