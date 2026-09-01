package main

import (
	"fmt"
	"strings"
	"unicode"
)

// initialisms are the words Go capitalises whole. The list is the one revive's
// var-naming rule enforces, and it is used verbatim rather than extended: a
// capitalisation this repository invented would be a name the linter is content
// with and a Go programmer is not, and the linter is the thing that keeps the
// rule from drifting.
var initialisms = map[string]string{
	"acl": "ACL", "api": "API", "ascii": "ASCII", "cpu": "CPU", "css": "CSS",
	"dns": "DNS", "eof": "EOF", "guid": "GUID", "html": "HTML", "http": "HTTP",
	"https": "HTTPS", "id": "ID", "ip": "IP", "json": "JSON", "lhs": "LHS",
	"qps": "QPS", "ram": "RAM", "rhs": "RHS", "rpc": "RPC", "sla": "SLA",
	"smtp": "SMTP", "sql": "SQL", "ssh": "SSH", "tcp": "TCP", "tls": "TLS",
	"ttl": "TTL", "udp": "UDP", "ui": "UI", "uid": "UID", "uuid": "UUID",
	"uri": "URI", "url": "URL", "utf8": "UTF8", "vm": "VM", "xml": "XML",
	"xmpp": "XMPP", "xsrf": "XSRF", "xss": "XSS",
}

// goName converts a schema name — a definition key, a property name or a union
// tag — into an exported Go identifier.
//
// The schema spells names three ways: PascalCase definitions, camelCase
// properties, and snake_case union tags. All three split into words the same
// way, and the result goes through the initialism list so that SessionId becomes
// SessionID and httpHeader becomes HTTPHeader.
func goName(name string) string {
	var out strings.Builder
	for _, word := range splitWords(name) {
		if fixed, ok := initialisms[strings.ToLower(word)]; ok {
			out.WriteString(fixed)
			continue
		}
		runes := []rune(word)
		out.WriteRune(unicode.ToUpper(runes[0]))
		out.WriteString(string(runes[1:]))
	}
	return out.String()
}

// unexport turns an exported Go name into an unexported one, keeping Go's
// initialism convention: HTTPHeader becomes httpHeader rather than hTTPHeader,
// because the leading run of capitals is one word and the last of them starts the
// next.
func unexport(name string) string {
	runes := []rune(name)
	leading := 0
	for leading < len(runes) && unicode.IsUpper(runes[leading]) {
		leading++
	}
	switch {
	case leading == 0:
		return name
	case leading == 1 || leading == len(runes):
		// One capital, or all of them: there is no following word to protect.
		for i := range min(leading, len(runes)) {
			runes[i] = unicode.ToLower(runes[i])
		}
	default:
		for i := range leading - 1 {
			runes[i] = unicode.ToLower(runes[i])
		}
	}
	return string(runes)
}

// splitWords breaks a name on case boundaries and on the separators the schema
// uses, dropping the leading underscore of _meta and the empty words that
// snake_case leaves behind.
func splitWords(name string) []string {
	var words []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, string(current))
			current = nil
		}
	}
	for _, r := range name {
		switch {
		case r == '_' || r == '-' || r == '/' || r == ' ' || r == '.':
			flush()
		case unicode.IsUpper(r):
			flush()
			current = append(current, unicode.ToLower(r))
		default:
			current = append(current, r)
		}
	}
	flush()
	return words
}

// A names allocates the Go identifiers the generated package exports, so that
// two schema constructs cannot claim one name and so that a name does not depend
// on how much of the schema is currently in scope.
//
// Every definition and every union arm in the whole schema is registered, not
// only the ones the manifest reaches. Otherwise growing the manifest would
// rename a type that was already published, and the rename would arrive as a
// surprise in a release-compatibility report rather than as a decision.
type names struct {
	owner map[string]string // Go name -> what owns it, for the collision message
}

func newNames() *names {
	return &names{owner: make(map[string]string)}
}

// claim registers name for owner, and fails if something else already has it.
func (n *names) claim(name, owner string) error {
	if existing, taken := n.owner[name]; taken {
		return fmt.Errorf("%s and %s both want the Go name %s", existing, owner, name)
	}
	n.owner[name] = owner
	return nil
}

// claimArm registers a generated union arm type. Its preferred name comes from
// the arm itself; when that is taken — SessionUpdate's plan_update arm would
// want PlanUpdate, which is the name of the payload it wraps — the union
// qualifies it.
func (n *names) claimArm(preferred, union, owner string) (string, error) {
	if _, taken := n.owner[preferred]; !taken {
		n.owner[preferred] = owner
		return preferred, nil
	}
	qualified := union + preferred
	if existing, taken := n.owner[qualified]; taken {
		return "", fmt.Errorf("%s and %s both want the Go name %s", existing, owner, qualified)
	}
	n.owner[qualified] = owner
	return qualified, nil
}
