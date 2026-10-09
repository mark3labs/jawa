# Live activity

The UI consumes authenticated GET /events using Datastar @get, not /activity
polling. The server renders keyed board/activity fragments and pushes changes
as datastar-patch-elements events. Mutations return element patches and dialog
close signals; action errors patch an escaped notice without navigation.

GET /activity remains available for JSON inspection and integration assertions.
See SSE_UI.md and UI_SSE.md for component roots and wire behavior.
History/worker disclosures use data-preserve-attr="open"; create/delete dialogs
are outside patched regions so drafts survive live updates.
