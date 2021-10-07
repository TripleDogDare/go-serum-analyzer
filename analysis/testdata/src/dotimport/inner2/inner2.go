package inner2

// ExportedFunc2 is a demo function.
//
// Errors:
//
//    - hello-error -- is always returned
func ExportedFunc2() error {
	return &Inner2Error{"hello-error"}
}

func CodeNotDeclared2() error {
	return &Inner2Error{"some-error"}
}

type SomeType2 struct{}

func (SomeType2) CodeNotDeclared() error {
	return &Inner2Error{"some-error"}
}

type Inner2Error struct {
	TheCode string
}

func (e *Inner2Error) Code() string               { return e.TheCode }
func (e *Inner2Error) Message() string            { return e.TheCode }
func (e *Inner2Error) Details() map[string]string { return nil }
func (e *Inner2Error) Cause() error               { return nil }
func (e *Inner2Error) Error() string              { return e.Message() }

type Inner2UnusedError struct {
	TheCode string
}

func (e *Inner2UnusedError) Code() string  { return e.TheCode }
func (e *Inner2UnusedError) Error() string { return e.TheCode }
