-- Fence durable operation ownership and record the boundary before pane input.
-- An old in_progress row cannot prove that no input was sent. Mark those
-- records started so a retry cannot repeat an unknown actuation. Completed
-- receipts retain their status, outcome, timestamps, and original binding.
ALTER TABLE send_operations ADD COLUMN claim_token TEXT NOT NULL DEFAULT '';
ALTER TABLE send_operations ADD COLUMN dispatch_started_at TIMESTAMP;
ALTER TABLE send_operations ADD COLUMN targets_json TEXT NOT NULL DEFAULT '[]';

UPDATE send_operations SET claim_token = lower(hex(randomblob(32)));
UPDATE send_operations
SET dispatch_started_at = created_at
WHERE status = 'in_progress';
