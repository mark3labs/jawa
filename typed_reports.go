package main

import "jawa/internal/reportbridge"

// One host-owned bridge connects managed Kit setup with the runtime supplied by
// the NATS channel factory. No identity or credentials are stored in model args.
// The adapter is shared with the production-path factory smoke host.
var trustedReports reportbridge.Bridge
