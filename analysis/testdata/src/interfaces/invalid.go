package interfaces

type InvalidSimpleImpl struct{}

// InterfaceMethod is a method used to implement SimpleInterface,
// but the declared (and actual) error codes are not a subset of the ones declared in the interface.
//
// Errors:
//
//    - unknown-error -- is always returned making InvalidSimpleImpl an invalid implementation of the interface
func (InvalidSimpleImpl) SimpleInterfaceMethod() error { // want SimpleInterfaceMethod:"ErrorCodes: unknown-error"
	return &Error{"unknown-error"}
}

func InvalidReturn() SimpleInterface {
	return InvalidSimpleImpl{} // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
}

func InvalidReturn2() (int, int, SimpleInterface) {
	return 5, 42, InvalidSimpleImpl{} // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
}

func InvalidReturn3() (_, _ int, _, _ SimpleInterface) {
	return 5, 42, nil, InvalidSimpleImpl{} // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
}

func InvalidAssignment() *SimpleInterface {
	var x SimpleInterface
	x = InvalidSimpleImpl{} // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	return &x
}

func InvalidAssignment2() {
	var x, y int
	var z SimpleInterface
	x, y, z = 7, 3, InvalidSimpleImpl{} // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	_, _, _ = x, y, z
}

func InvalidAssignment3() (x int, y, z SimpleInterface) {
	x = 5
	y = nil
	z = InvalidSimpleImpl{} // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	return
}

func ReturnTwoInvalidSimpleImpl() (InvalidSimpleImpl, InvalidSimpleImpl) {
	return InvalidSimpleImpl{}, InvalidSimpleImpl{}
}

func InvalidAssignment4() {
	var y SimpleInterface
	x, y := ReturnTwoInvalidSimpleImpl() // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	_, _ = x, y

	var u, v SimpleInterface
	u, v = ReturnTwoInvalidSimpleImpl() /*
		want
			`cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
			`cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]` */
	_, _ = u, v
}

func TakingSimpleInterface(_, _ int, _ SimpleInterface) {}

func TakingSimpleInterfaceVariadic(_ int, _ ...SimpleInterface) {}

func TakingSimpleInterfaceSlice(_ int, _ []SimpleInterface) {}

func InvalidFunctionCall() {
	param := InvalidSimpleImpl{}
	TakingSimpleInterface(42, 42, param) // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	TakingSimpleInterface(42, 42, nil)
	TakingSimpleInterfaceVariadic(-5, nil, param)      // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	TakingSimpleInterfaceVariadic(-5, param, nil, nil) // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	TakingSimpleInterfaceVariadic(-5, nil, nil, nil)

	// Make sure slice parameters don't get confused with variadic.
	TakingSimpleInterfaceSlice(7, []SimpleInterface{nil, nil})
	TakingSimpleInterfaceSlice(7, nil)

	var slice []SimpleInterface
	slice = append(slice, param)           // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	slice = append(slice, nil, nil, param) // want `cannot use expression as "SimpleInterface" value: method "SimpleInterfaceMethod" declares the following error codes which were not part of the interface: \[unknown-error]`
	slice = append(slice, nil, nil)
}
