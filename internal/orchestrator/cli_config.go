package orchestrator

import (
	"github.com/spf13/cobra"
	"os"
)

// providerFlags supplies command-local defaults; dashboard settings still win.
func providerFlags(cmd *cobra.Command, values map[string]string) {
	for _, spec := range []struct{ flag, env string }{{"github-token", "JAWA_GITHUB_TOKEN"}, {"forgejo-token", "JAWA_FORGEJO_TOKEN"}} {
		value := effectiveProviderSetting(nil, spec.flag, spec.env)
		values[spec.flag] = value
		key := spec.flag
		cmd.Flags().String(key, value, "Provider credential (default from "+spec.env+"); never displayed")
		cmd.Flags().Lookup(key).DefValue = ""
	}
	cmd.Flags().Bool("allow-no-ci", os.Getenv("JAWA_ALLOW_NO_CI") == "true", "Allow missing GitHub CI evidence (JAWA_ALLOW_NO_CI)")
}

func readProviderFlags(cmd *cobra.Command, values map[string]string) {
	for _, key := range []string{"github-token", "forgejo-token", "allow-no-ci"} {
		values[key] = cmd.Flags().Lookup(key).Value.String()
	}
}

func secretFlag(cmd *cobra.Command, target *string, name, env, fallback string) {
	cmd.Flags().StringVar(target, name, orchestratorEnv(env, fallback), "Connection setting ("+env+"); default not displayed")
	cmd.Flags().Lookup(name).DefValue = ""
}
