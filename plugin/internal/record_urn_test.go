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
}
