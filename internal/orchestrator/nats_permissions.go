package orchestrator

import "github.com/nats-io/nats-server/v2/server"

// Credentialed agents are trusted. Consumer creation remains enabled for
// BONNIE task consumers and KV watches; JetStream delivery proxies can bypass
// these direct subject restrictions. This is not hostile-agent isolation.
func workerPermissions() *server.Permissions {
	return &server.Permissions{
		Publish: &server.SubjectPermission{Allow: []string{">"}, Deny: []string{
			"bonnie.api.>", "$KV.jawa_control.>",
			"$JS.API.STREAM.CREATE.>", "$JS.API.STREAM.UPDATE.>",
			"$JS.API.STREAM.DELETE.KV_jawa_control", "$JS.API.STREAM.PURGE.KV_jawa_control", "$JS.API.STREAM.MSG.DELETE.KV_jawa_control",
			"$JS.API.STREAM.SNAPSHOT.>", "$JS.API.STREAM.RESTORE.>",
		}},
		Subscribe: &server.SubjectPermission{Allow: []string{">"}, Deny: []string{"_INBOX.bonnieapi.>"}},
	}
}
