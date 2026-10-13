-- +goose Up
-- A final failed outcome could store run_state='running' when BONNIE reported
-- an error before its run snapshot left running. Status events no longer apply
-- after a final outcome, so those attempts looked active forever and blocked
-- Retry, Reset, Delete, and Cancel. Mark their run as failed.
UPDATE workflow_attempts SET run_state='failed'
WHERE state='failed' AND ready=0
  AND run_state IN ('pending','queued','submitted','running')
  AND outcome_json<>'' AND json_valid(outcome_json)
  AND json_extract(outcome_json,'$.state') IN ('pending','queued','submitted','running')
  AND coalesce(json_extract(outcome_json,'$.error'),'')<>'';
-- +goose Down
SELECT 1;
