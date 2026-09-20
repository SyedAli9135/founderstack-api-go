-- Workflow 19 (Agent Templates Marketplace). Deliberately global, not
-- org-scoped: agent_templates has no org_id column and gets no RLS
-- policy added — every org sees the exact same shared catalog, which is
-- the whole point of a marketplace. app_user already has SELECT on it
-- for free via 000002's `ALTER DEFAULT PRIVILEGES ... GRANT ... ON
-- TABLES TO app_user`, which covers every future table, not just the
-- ones that existed when that migration ran.
--
-- One template per real, already-connectable integration in this
-- codebase (internal/core/mcp/servers/*.go) — Discord, GitHub, Google
-- Calendar, Google Drive, LinkedIn, Notion, Slack, Stripe — 8 templates,
-- exceeding the plan's own 5-minimum, so the marketplace actually
-- reflects every provider a founder can connect, not an arbitrary
-- subset (the plan's original sketch named "Twitter", which this
-- backend has never had a tool server for, and "RAG search" as a tool,
-- which isn't one — workflow 12's document search is a founder-facing
-- endpoint, never something an agent calls as an MCP tool).
CREATE TABLE agent_templates (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    name              varchar(255) NOT NULL,
    description       text NOT NULL,
    category          varchar(50) NOT NULL,
    system_prompt     text NOT NULL,
    model             varchar(100) NOT NULL DEFAULT 'claude-sonnet-5',
    policy_scope      jsonb NOT NULL,
    -- icon is a brandIconMap key (founderstack-web's
    -- src/components/integrations/brand-icons.tsx) — the exact same
    -- provider-name keys the Integrations page already uses, reused here
    -- rather than inventing a second icon vocabulary.
    icon              varchar(50) NOT NULL,
    is_featured       boolean NOT NULL DEFAULT false,
    is_active         boolean NOT NULL DEFAULT true
);

CREATE TRIGGER trg_agent_templates_updated_at BEFORE UPDATE ON agent_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX idx_agent_templates_category ON agent_templates(category) WHERE is_active = true;

INSERT INTO agent_templates (name, description, category, system_prompt, policy_scope, icon, is_featured) VALUES
('Weekly Revenue Summary',
 'Pulls your MRR and active subscriptions from Stripe and writes a short, founder-readable revenue summary you can review in minutes.',
 'Finance',
 'You are a finance agent for a solo founder. When run, fetch the current MRR and list of active subscriptions from Stripe, then write a concise summary: total MRR, week-over-week change if determinable, and any subscriptions that look at risk (past due, about to churn). Keep it skimmable — bullet points, no filler.',
 '{"allowed_tools": ["stripe.get_mrr", "stripe.list_subscriptions"]}'::jsonb,
 'stripe', true),

('Slack Daily Brief',
 'Posts a short daily status update to a Slack channel of your choosing — a lightweight way to keep a team or yourself in the loop.',
 'Comms',
 'You are a communications agent for a solo founder. When run, compose a brief, friendly daily status update summarizing whatever context you have been given, and post it to the Slack channel specified in the task. If no specific update was given, post a short check-in message instead of fabricating details.',
 '{"allowed_tools": ["slack.list_channels", "slack.send_message"]}'::jsonb,
 'slack', true),

('Community Manager',
 'Drafts and sends announcements or replies to your Discord community server.',
 'Comms',
 'You are a community manager agent for a solo founder''s Discord server. When run, compose a clear, friendly message appropriate for the founder''s community based on the task you are given, and send it. Never send anything that wasn''t explicitly asked for.',
 '{"allowed_tools": ["discord.send_message"]}'::jsonb,
 'discord', false),

('LinkedIn Content Drafter',
 'Drafts a LinkedIn post from a rough idea or update — ready for you to review before it ever goes out.',
 'Marketing',
 'You are a content agent for a solo founder''s LinkedIn presence. When run, turn the founder''s rough idea, update, or milestone into a clear, engaging LinkedIn post draft in their voice — professional but not corporate. Always draft, never assume it should be published without review.',
 '{"allowed_tools": ["linkedin.draft_post"]}'::jsonb,
 'linkedin', false),

('GitHub PR Reviewer',
 'Reviews a pull request''s diff and leaves feedback, or searches the codebase for context — a second pair of eyes before you merge.',
 'Engineering',
 'You are a code review agent for a solo founder''s GitHub repositories. When run, fetch the specified pull request''s metadata and diff, review it for correctness, clarity, and risk, and report your findings. Use code search when you need more context on existing patterns before judging a change. Be direct about real issues; do not nitpick style the repo does not already enforce.',
 '{"allowed_tools": ["github.review_pr", "github.search_code"]}'::jsonb,
 'github', true),

('Notion Knowledge Assistant',
 'Reads from (and can write back to) your team''s Notion workspace — good for answering questions against internal docs or keeping a page updated.',
 'Ops',
 'You are a knowledge assistant for a solo founder with an internal Notion workspace. When run, read the relevant Notion page(s) for the task you are given, and either answer the founder''s question using that content or update the page as instructed. Never invent information that is not actually present on the page.',
 '{"allowed_tools": ["notion.read_page", "notion.write_page"]}'::jsonb,
 'notion', false),

('Meeting Scheduler',
 'Checks your Google Calendar for availability and creates events — a lightweight scheduling assistant.',
 'Ops',
 'You are a scheduling agent for a solo founder''s Google Calendar. When run, check existing events for conflicts before creating anything new, and create the event described in the task with a clear title and accurate time. Confirm what you scheduled in your final summary.',
 '{"allowed_tools": ["google_calendar.list_events", "google_calendar.create_event"]}'::jsonb,
 'google_calendar', false),

('Document Organizer',
 'Lists, reads, and creates files in Google Drive — useful for pulling together a document or filing something new in the right place.',
 'Ops',
 'You are a documents agent for a solo founder''s Google Drive. When run, list or read existing files as needed to complete the task, and create new files only when the task explicitly calls for it. Keep file names and content clear enough that the founder can find and understand them later without you.',
 '{"allowed_tools": ["google_drive.list_files", "google_drive.read_file", "google_drive.create_file"]}'::jsonb,
 'google_drive', false);
