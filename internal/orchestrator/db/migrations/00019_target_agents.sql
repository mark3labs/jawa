-- +goose Up
ALTER TABLE workflow_attempts ADD COLUMN target_agent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE planning_turns ADD COLUMN target_agent_id TEXT NOT NULL DEFAULT '';
