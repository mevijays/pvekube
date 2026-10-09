-- Set once a failed/cancelled/interrupted template build's leftovers (the
-- Packer container and the build VM on Proxmox) have been confirmed removed,
-- either automatically at the end of the job or by the operator's "Clean up"
-- action. Unset means the Templates page should still offer that clean-up.
ALTER TABLE jobs ADD COLUMN cleaned_at TIMESTAMP;
