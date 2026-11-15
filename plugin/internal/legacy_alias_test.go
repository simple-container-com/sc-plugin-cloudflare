package cloudflare

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestLegacyClaims_OneRecordPerLegacyName(t *testing.T) {
	RegisterTestingT(t)

	var c legacyClaims
	Expect(c.claim("host.example.com")).To(BeTrue(), "first A or AAAA at a host takes the alias")
	Expect(c.claim("host.example.com")).To(BeFalse(), "a second record at the host must not alias the same URN")
	Expect(c.claim("other.example.com")).To(BeTrue(), "another host is independent")
}
