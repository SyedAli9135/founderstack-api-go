-- Workflow 19 follow-up: migration 000015 seeded one template per real
-- integration (8), but that's a different axis than "every real tool" —
-- Stripe and GitHub each have write-side tools no template exercised
-- (stripe.create_invoice/refund_payment, github.create_issue), found by
-- literally diffing the seeded catalog's tool set against
-- internal/core/mcp/servers/*.go's full 18-tool surface after the founder
-- asked "have we added all possible templates?" — a real, useful
-- question, not a hypothetical. These 2 close that gap to 18/18.
INSERT INTO agent_templates (name, description, category, system_prompt, policy_scope, icon, is_featured) VALUES
('Invoice & Billing Assistant',
 'Drafts invoices and processes refunds through Stripe when you ask it to — the action side of billing, distinct from Weekly Revenue Summary''s read-only reporting.',
 'Finance',
 'You are a billing agent for a solo founder''s Stripe account. When run, create the invoice or process the refund described in the task, using exactly the amounts and customer specified — never invent or round a figure. Confirm what you did (and its result) clearly in your final summary; these are real financial actions.',
 '{"allowed_tools": ["stripe.create_invoice", "stripe.refund_payment"]}'::jsonb,
 'stripe', false),

('Issue Triager',
 'Searches your codebase for context and files a properly-described GitHub issue — good for turning a bug report or feature idea into a real, actionable ticket.',
 'Engineering',
 'You are an issue-triage agent for a solo founder''s GitHub repositories. When run, search the codebase for relevant context on the bug or feature request you are given, then file a clear GitHub issue: a descriptive title, a body explaining the problem/request and any context you found, and nothing invented beyond what the task actually described.',
 '{"allowed_tools": ["github.create_issue", "github.search_code"]}'::jsonb,
 'github', false);
