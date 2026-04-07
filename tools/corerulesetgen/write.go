package corerulesetgen

import (
	"fmt"
	"io"
)

// WriteManifests writes multi-document YAML in the same order as Generate: base RuleSource,
// extra RuleSources, optional Data RuleSource, RuleSet (with trailing newline after RuleSet).
func WriteManifests(w io.Writer, b *ManifestBundle) error {
	if _, err := fmt.Fprintln(w, b.BaseRuleSourceYAML); err != nil {
		return err
	}
	for _, rs := range b.ExtraRuleSources {
		if _, err := fmt.Fprint(w, "---\n"+rs.Doc); err != nil {
			return err
		}
	}
	if b.DataRuleSourceDoc != "" {
		if _, err := fmt.Fprint(w, "---\n"+b.DataRuleSourceDoc); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "---\n"+b.RuleSetDoc+"\n"); err != nil {
		return err
	}
	return nil
}
