package main

import "jawa/internal/reportbridge"

// One host-owned bridge connects managed Kit setup with the runtime supplied by
// the NATS channel factory. No identity or credentials are stored in model args.
// The adapter is shared with the production-path factory smoke host. Planning
// ACKs map to actual BONNIE suspension values; terminal ACKs retain Request.
var trustedReports reportbridge.Bridge
