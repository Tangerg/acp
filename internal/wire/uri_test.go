package wire_test

import (
	"testing"

	"github.com/Tangerg/acp/internal/wire"
)

// These cases pin generic URI syntax, including cases where WHATWG URL parsing
// normalizes invalid input or applies stricter scheme-specific requirements.
// The boundary cases follow JSON-Schema-Test-Suite's draft2020-12/optional/format/uri.json.
func TestURIUsesRFC3986Syntax(t *testing.T) {
	for _, value := range []string{
		"http://foo.bar/?baz=qux#quux",
		"http://foo.com/blah_(wikipedia)_blah#cite-1",
		"http://foo.bar/?q=Test%20URL-encoded%20stuff",
		"http://xn--nw2a.xn--j6w193g/",
		"http://-.~_!$&'()*+,;=:%40:80%2f::::::@example.com",
		"http://223.255.255.254",
		"ftp://ftp.is.co.za/rfc/rfc1808.txt",
		"http://www.ietf.org/rfc/rfc2396.txt",
		"ldap://[2001:db8::7]/c=GB?objectClass?one",
		"mailto:John.Doe@example.com",
		"news:comp.infosystems.www.servers.unix",
		"tel:+1-816-555-1212",
		"urn:oasis:names:specification:docbook:dtd:xml:4.1.2",
		"http://087.10.0.1/",
		"http://999.999.999.999/",
		"https:",
		"urn:",
		"x:///path",
		"x://@:",
		"x://host:999999999999999999999999/",
		"x://[v1.fe80:abcd]/",
		"x://[VfF.address]/",
		"http://%65xample.com/%CE%BB?x=%E7%95%8C#%20",
		"http://[::ffff:192.0.2.1]/",
		"x:path?next=a/b?c#fragment?part",
	} {
		t.Run("valid/"+value, func(t *testing.T) {
			if err := wire.ValidateURI(value); err != nil {
				t.Fatalf("valid URI rejected: %v", err)
			}
		})
	}
	for _, value := range []string{
		"",
		"//foo.bar/?baz=qux#quux",
		"/abc",
		`\\WINDOWS\fileshare`,
		"abc",
		"http:// shouldfail.com",
		":// should fail",
		"bar,baz:foo",
		"https://[@example.org/test.txt",
		"https://example.org/foobar®.txt",
		`https://example.org/foobar\.txt`,
		`https://example.org/foobar".txt`,
		"https://example.org/foobar<>.txt",
		"https://example.org/foobar{}.txt",
		"https://example.org/foobar^.txt",
		"https://example.org/foobar`.txt",
		"https://example.org/foo bar.txt",
		"https://example.org/foobar|.txt",
		"http://example.com/%6G",
		"http://example.com/%A",
		"http://example.com/%",
		"1http://example.com",
		"ht_tp://example.com",
		"http://example.com:abc/path",
		"http://[::ffff:01.2.3.4]",
		"http:/[::1]",
		"http://foo.bar/\n",
		"http://2001:0db8:85a3:0000:0000:8a2e:0370:7334",
		"http://[invalid]/",
		"http://[127.0.0.1]/",
		"http://[fe80::1%25eth0]/",
		"x://[v.fe80]/",
		"x://[vG.fe80]/",
		"x://[v1.]/",
		"x://[v1.a%20b]/",
		"x://[::1]suffix/",
		"http://user@other@host/",
		"http://example.com/?x=%ZZ",
		"http://example.com/#%",
		"http://example.com/#a#b",
		"http://example.com/?x=[1]",
	} {
		t.Run("invalid/"+value, func(t *testing.T) {
			if err := wire.ValidateURI(value); err == nil {
				t.Fatal("invalid URI accepted")
			}
		})
	}
}
