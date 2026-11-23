package wire

import (
	"errors"
	"net/netip"
	"strings"
)

const (
	uriUnreservedPunctuation = "-._~"
	uriSubDelimiters         = "!$&'()*+,;="
	uriColonDelimiter        = ":"
	uriPathDelimiters        = ":@/"
	uriQueryDelimiters       = ":@/?"
	uriPercentEscapeLength   = 3
)

// ValidateURI follows RFC 3986's generic grammar because JSON Schema's uri
// format does not imply WHATWG URL normalization or scheme-specific checks.
// Unicode must be percent-encoded, IPvFuture is admissible, and a numeric host
// that is not an IPv4 address may still be a valid registered name.
func ValidateURI(value string) error {
	scheme, remainder, present := strings.Cut(value, ":")
	if !present || !validURIScheme(scheme) {
		return errors.New("URI requires a valid scheme")
	}
	hierarchy, fragment, hasFragment := strings.Cut(remainder, "#")
	if hasFragment && !validURIComponent(fragment, uriQueryDelimiters) {
		return errors.New("URI has an invalid fragment")
	}
	path, query, hasQuery := strings.Cut(hierarchy, "?")
	if hasQuery && !validURIComponent(query, uriQueryDelimiters) {
		return errors.New("URI has an invalid query")
	}
	if authorityAndPath, hasAuthority := strings.CutPrefix(path, "//"); hasAuthority {
		authority, relativePath, _ := strings.Cut(authorityAndPath, "/")
		if !validURIAuthority(authority) {
			return errors.New("URI has an invalid authority")
		}
		path = relativePath
	}
	if !validURIComponent(path, uriPathDelimiters) {
		return errors.New("URI has an invalid path")
	}
	return nil
}

func validURIScheme(scheme string) bool {
	if len(scheme) == 0 || !asciiLetter(scheme[0]) {
		return false
	}
	for index := 1; index < len(scheme); index++ {
		character := scheme[index]
		if !asciiLetter(character) && !asciiDigit(character) &&
			character != '+' && character != '-' && character != '.' {
			return false
		}
	}
	return true
}

func validURIAuthority(authority string) bool {
	hostPort := authority
	if userInfo, host, present := strings.Cut(authority, "@"); present {
		if !validURIComponent(userInfo, uriColonDelimiter) {
			return false
		}
		hostPort = host
	}
	if literal, bracketed := strings.CutPrefix(hostPort, "["); bracketed {
		host, suffix, closed := strings.Cut(literal, "]")
		if !closed || !validURIIPLiteral(host) {
			return false
		}
		if suffix == "" {
			return true
		}
		port, present := strings.CutPrefix(suffix, ":")
		return present && validURIPort(port)
	}
	host, port, hasPort := strings.Cut(hostPort, ":")
	return validURIComponent(host, "") && (!hasPort || validURIPort(port))
}

func validURIIPLiteral(host string) bool {
	if len(host) > 0 && (host[0] == 'v' || host[0] == 'V') {
		version, address, present := strings.Cut(host[1:], ".")
		if !present || len(version) == 0 || len(address) == 0 || strings.Contains(address, "%") {
			return false
		}
		for index := range len(version) {
			if !asciiHexDigit(version[index]) {
				return false
			}
		}
		return validURIComponent(address, uriColonDelimiter)
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.Is6() && address.Zone() == ""
}

func validURIPort(port string) bool {
	for index := range len(port) {
		if !asciiDigit(port[index]) {
			return false
		}
	}
	return true
}

// Escapes are checked in every component, including query and fragment, which
// net/url.Parse deliberately retains without validating their percent triplets.
func validURIComponent(value, delimiters string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '%' {
			if len(value)-index < uriPercentEscapeLength || !asciiHexDigit(value[index+1]) || !asciiHexDigit(value[index+2]) {
				return false
			}
			index += uriPercentEscapeLength - 1
			continue
		}
		if !asciiLetter(character) && !asciiDigit(character) &&
			!strings.ContainsRune(uriUnreservedPunctuation, rune(character)) &&
			!strings.ContainsRune(uriSubDelimiters, rune(character)) &&
			!strings.ContainsRune(delimiters, rune(character)) {
			return false
		}
	}
	return true
}

func asciiLetter(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func asciiDigit(character byte) bool {
	return character >= '0' && character <= '9'
}

func asciiHexDigit(character byte) bool {
	return asciiDigit(character) || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F'
}
