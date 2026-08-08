package secret

// Value holds a sensitive string (API key, token, credential). Its zero
// value is an empty, "unset" secret.
type Value string

// String implements fmt.Stringer so %s/%v and naive logging never print the
// underlying secret.
func (v Value) String() string {
	if v == "" {
		return ""
	}
	return "***REDACTED***"
}

// GoString implements fmt.GoStringer so %#v (struct dumps, debuggers) is
// also redacted.
func (v Value) GoString() string {
	return "secret.Value(\"" + v.String() + "\")"
}

// Expose returns the underlying plaintext value. Call sites should be
// narrow and deliberate (e.g. building an Authorization header).
func (v Value) Expose() string {
	return string(v)
}

// IsEmpty reports whether no value was configured.
func (v Value) IsEmpty() bool {
	return v == ""
}
