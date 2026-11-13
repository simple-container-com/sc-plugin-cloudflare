package cloudflare

import (
	"testing"

	. "github.com/onsi/gomega"

	"github.com/simple-container-com/api/pkg/api"
)

// 2026-11-04 — #13 renamed every per-record URN without an alias, so stacks
// deployed before it planned a create for a CNAME Cloudflare already held
// (81053). A CNAME keeps one URN per host, aliased to the pre-#13 name.
func TestRecordURNName_CNAMEIsStablePerHostAndAliasesTheLegacyName(t *testing.T) {
	RegisterTestingT(t)
	first, opts := recordURNName(api.DnsRecord{Name: "_abc.app.example.com.", Type: "CNAME", Value: "_one.acm-validations.aws."})
	reissued, _ := recordURNName(api.DnsRecord{Name: "_abc.app.example.com.", Type: "CNAME", Value: "_two.acm-validations.aws."})
	Expect(first).To(Equal("_abc.app.example.com.-CNAME-record"))
	Expect(reissued).To(Equal(first), "a changed CNAME target must update in place, not create a second record at the host")
	Expect(opts).To(HaveLen(1), "the CNAME must carry the alias to its pre-#13 name")
}

func TestRecordURNName_RepeatableTypesKeepTheValueHash(t *testing.T) {
	RegisterTestingT(t)
	a, aOpts := recordURNName(api.DnsRecord{Name: "@", Type: "MX", Value: "10 mail.one."})
	b, _ := recordURNName(api.DnsRecord{Name: "@", Type: "MX", Value: "20 mail.two."})
	Expect(a).NotTo(Equal(b), "two MX records at one name need distinct URNs (#13)")
	Expect(a).To(MatchRegexp(`^@-MX-[0-9a-f]{8}-record$`))
	Expect(aOpts).To(BeEmpty(), "several records may share a legacy name, so repeatable types carry no alias")

	txt, txtOpts := recordURNName(api.DnsRecord{Name: "_dmarc", Type: "TXT", Value: "v=DMARC1"})
	Expect(txt).To(MatchRegexp(`^_dmarc-TXT-[0-9a-f]{8}-record$`))
	Expect(txtOpts).To(BeEmpty())
}

// #13 renamed an A record from "<name>-record" with no alias, so a stack deployed
// before it planned to delete its live record and create an identical one under
// the new name. Found on a PAY.SPACE staging preview: a plan with the record
// "landing-staging.tech-pay.space-record" deleted and
// "landing-staging.tech-pay.space-A-<hash>-record" created.
func TestRecordURNName_AAndAAAAAliasTheirLegacyName(t *testing.T) {
	RegisterTestingT(t)

	for _, typ := range []string{"A", "AAAA", "a"} {
		name, opts := recordURNName(api.DnsRecord{Name: "landing-staging.tech-pay.space", Type: typ, Value: "34.88.1.2"})
		Expect(name).To(MatchRegexp(`^landing-staging\.tech-pay\.space-(A|AAAA|a)-[0-9a-f]{8}-record$`), typ)
		Expect(opts).To(HaveLen(1), typ+" must alias the pre-#13 name")
	}

	// The new name still tells records of one name apart: a changed address
	// is a different URN, not an in-place update of the aliased one.
	one, _ := recordURNName(api.DnsRecord{Name: "h", Type: "A", Value: "1.1.1.1"})
	two, _ := recordURNName(api.DnsRecord{Name: "h", Type: "A", Value: "2.2.2.2"})
	Expect(one).NotTo(Equal(two))
}
