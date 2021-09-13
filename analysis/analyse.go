package analysis

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

var logf = fmt.Printf

// var logf = func(_ string, _ ...interface{}) {}

var VerifyAnalyzer = &analysis.Analyzer{
	Name:      "reeverify",
	Doc:       "Checks that any function that has a ree-style docstring enumerating error codes is telling the truth.",
	Requires:  []*analysis.Analyzer{inspect.Analyzer},
	Run:       runVerify,
	FactTypes: []analysis.Fact{new(ErrorCodes)},
}

type ErrorCodes struct {
	Codes []string
}

func (*ErrorCodes) AFact() {}

func (e *ErrorCodes) String() string {
	sort.Strings(e.Codes)
	return fmt.Sprintf("ErrorCodes: %v", strings.Join(e.Codes, " "))
}

// isErrorCodeValid checks if the given error code is valid.
// Valid error codes have to match against: "^[a-zA-Z][a-zA-Z0-9\-]*[a-zA-Z0-9]$" or "^[a-zA-Z]$".
func isErrorCodeValid(code string) bool {
	if len(code) == 0 {
		return false
	}

	// Verify that first and last char do not contain invalid values.
	if code[0] == '-' || (code[0] >= '0' && code[0] <= '9') {
		return false
	}
	if code[len(code)-1] == '-' {
		return false
	}

	// Verify that the remaining chars match [a-zA-Z0-9\-]
	for _, c := range code {
		if !(c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}

	return true
}

// FUTURE: may add another analyser that is "ree-exhaustive".

func runVerify(pass *analysis.Pass) (interface{}, error) {
	funcsToAnalyse := findErrorReturningFunctions(pass)

	// First output: warn directly about any functions that are exported
	// if they return errors, but don't declare error codes in their docs.
	// Also: because we have to do the whole parse for docstrings already,
	// remember the error codes for the funcs that do have them:
	// those are what we'll look at for the remaining analysis.
	funcClaims := map[*ast.FuncDecl]codeSet{}
	for _, funcDecl := range funcsToAnalyse {
		codes, err := findErrorDocs(funcDecl)
		if err != nil {
			pass.Reportf(funcDecl.Pos(), "function %q has odd docstring: %s", funcDecl.Name.Name, err)
			continue
		}
		if len(codes) == 0 {
			if funcDecl.Name.IsExported() {
				pass.Reportf(funcDecl.Pos(), "function %q is exported, but does not declare any error codes", funcDecl.Name.Name)
			}
		} else {
			funcClaims[funcDecl] = codes
			logf("function %q declares error codes %s\n", funcDecl.Name.Name, codes)
		}
	}
	logf("%d functions in this package return errors and declared codes for them, and will be further analysed.\n\n", len(funcClaims))

	// Export all claimed error codes as facts.
	// Missing error code docs or unused ones will get reported in the respective functions,
	// but on caller site only the documented behaviour matters.
	for funcDecl, codeSet := range funcClaims {
		fn, ok := pass.TypesInfo.Defs[funcDecl.Name].(*types.Func)
		if !ok {
			logf("Could not find definition for function %q!", funcDecl.Name.Name)
			continue
		}

		fact := ErrorCodes{codeSet.slice()}
		pass.ExportObjectFact(fn, &fact)
	}

	// Okay -- let's look at the functions that have made claims about their error codes.
	// We'll explore deeply to find everything that can actually affect their error return value.
	// When we reach data initialization... we look at if those types implement coded errors, and try to figure out what their actual code value is.
	// When we reach other function calls that declare their errors, that's good enough info (assuming they're also being checked for truthfulness).
	// Anything else is trouble.
	for funcDecl, claimedCodes := range funcClaims {
		affectOrigins, foundCodes := findAffectorsOfErrorReturnInFunc(pass, funcDecl)
		logf("trace found these additional origins of error data...\n")
		for _, affector := range affectOrigins {
			logf(" - %s -- %s -- %v\n", pass.Fset.PositionFor(affector.Pos(), true), affector, checkErrorTypeHasLegibleCode(pass, affector))
		}
		logf("end of found origins.\n")
		var affectorCodes codeSet
		for _, affector := range affectOrigins {
			if checkErrorTypeHasLegibleCode(pass, affector) {
				// TMP:
				logf("Analysing error: %#v\n", pass.TypesInfo.Types[affector].Type)
				constantCodes, _ := analyseErrorType(pass, pass.TypesInfo.Types[affector].Type)
				affectorCodes = union(affectorCodes, constantCodes)

				codes := extractErrorCodes(pass, affector, funcDecl)
				affectorCodes = union(affectorCodes, codes)
			} else {
				pass.ReportRangef(affector, "expression does not define an error code")
			}
		}
		foundCodes = union(foundCodes, affectorCodes)
		logf("trace found error codes: %v\n", foundCodes)

		missingCodes := difference(foundCodes, claimedCodes).slice()
		unusedCodes := difference(claimedCodes, foundCodes).slice()
		var errorMessages []string
		if len(missingCodes) != 0 {
			sort.Strings(missingCodes)
			errorMessages = append(errorMessages, fmt.Sprintf("missing codes: %v", missingCodes))
		}
		if len(unusedCodes) != 0 {
			sort.Strings(unusedCodes)
			errorMessages = append(errorMessages, fmt.Sprintf("unused codes: %v", unusedCodes))
		}
		logf("\n")

		if len(errorMessages) != 0 {
			errorMessage := strings.Join(errorMessages, " ")
			pass.Reportf(funcDecl.Pos(), "function %q has a mismatch of declared and actual error codes: %s", funcDecl.Name.Name, errorMessage)
		}
	}

	return nil, nil
}

var tError = types.NewInterfaceType([]*types.Func{
	types.NewFunc(token.NoPos, nil, "Error", types.NewSignature(nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "", types.Typ[types.String])), false)),
}, nil).Complete()

var tReeError = types.NewInterfaceType([]*types.Func{
	types.NewFunc(token.NoPos, nil, "Error", types.NewSignature(nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "", types.Typ[types.String])), false)),
	types.NewFunc(token.NoPos, nil, "Code", types.NewSignature(nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "", types.Typ[types.String])), false)),
}, nil).Complete()

// findErrorReturningFunctions looks for functions that return an error,
// and emits a diagnostic if a function returns an error, but not as the last argument.
func findErrorReturningFunctions(pass *analysis.Pass) []*ast.FuncDecl {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// We only need to see function declarations at first; we'll recurse ourselves within there.
	nodeFilter := []ast.Node{
		(*ast.FuncDecl)(nil),
	}

	// Let's look only at functions that return errors;
	// and furthermore, errors as their last result (that's a normal enough convention, isn't it?).
	//
	// Returning more than one error will result in anything but the last one not being analysed.
	// Returning an error in any result field but the last one will result in it not being analysed.
	//
	// We'll actually look for anything that _implements_ `error` (!), not just the literal type.
	// Sometimes these will also, furthermore, perhaps implement our own extended error interface...
	// but if so, that's something we'll look into more later, not right now.
	var funcsToAnalyse []*ast.FuncDecl
	inspect.Preorder(nodeFilter, func(node ast.Node) {
		funcDecl := node.(*ast.FuncDecl)
		resultsList := funcDecl.Type.Results
		if resultsList == nil {
			return
		}
		lastResult := resultsList.List[len(resultsList.List)-1]
		typ := pass.TypesInfo.Types[lastResult.Type].Type
		if !types.Implements(typ, tError) {
			// Emit diagnostic if an error is returned as non-last argument
			for _, result := range resultsList.List {
				typ := pass.TypesInfo.Types[result.Type].Type
				if types.Implements(typ, tError) {
					pass.Reportf(result.Pos(), "error should be returned as the last argument")
				}
			}
			return
		}
		logf("function %q returns an error interface (type name: %q)\n", funcDecl.Name.Name, typ)
		funcsToAnalyse = append(funcsToAnalyse, funcDecl)
	})
	logf("%d functions in this package return errors and will be analysed.\n\n", len(funcsToAnalyse))

	return funcsToAnalyse
}

func findErrorDocs(funcDecl *ast.FuncDecl) (codeSet, error) {
	if funcDecl.Doc == nil {
		return nil, nil
	}
	return findErrorDocsSM{}.run(funcDecl.Doc.Text())
}

// findAffectorsInFunc looks up what can affect the given expression
// (which, generally, can be anything you'd expect to see in a ReturnStmt -- so, variables, unaryExpr, a bunch of things...),
// and recurses in this until it hits either the creation of a value,
// or function call boundaries (`*ast.CallExpr`).
//
// So, it'll follow any number of assignment statements, for example;
// as it does so, it'll totally disregarding logical branching,
// instead using a very basic model of taint: just marking anything that can ever possibly touch the variable.
//
func findAffectorsInFunc(pass *analysis.Pass, expr ast.Expr, within *ast.FuncDecl) (result []ast.Expr) {
	switch exprt := expr.(type) {
	case *ast.CallExpr: // These are a boundary condition, so that's short and sweet.
		return []ast.Expr{expr}
	case *ast.Ident: // Lovely!  These are easy.  (Although likely to have significant taint spread.)
		// Look for for `*ast.AssignStmt` in the function that could've affected this.
		ast.Inspect(within, func(node ast.Node) bool {
			// n.b., do *not* filter out *`ast.FuncLit`: statements inside closures can assign things!
			switch stmt2 := node.(type) {
			case *ast.AssignStmt:
				// Look for our ident's object in the left-hand-side of the assign.
				// Either follow up on the statement at the same index in the Rhs,
				// or watch out for a shorter Rhs that's just a CallExpr (i.e. it's a destructuring assignment).
				for i, clause := range stmt2.Lhs {
					switch clauset := clause.(type) {
					case *ast.Ident:
						if clauset.Obj == exprt.Obj {
							if len(stmt2.Lhs) > len(stmt2.Rhs) {
								// Destructuring mode.
								// We're going to make some crass simplifications here, and say... if this is anything other than the last arg, you're not supported.
								if i != len(stmt2.Lhs)-1 {
									pass.Reportf(clauset.Pos(), "unsupported: tracking error codes for function call with error as non-last return argument")
									return false
								}
								// Because it's a CallExpr, we're done here: this is part of the result.
								switch stmt2.Rhs[0].(type) {
								case *ast.CallExpr:
									result = append(result, stmt2.Rhs[0])
								default:
									panic("what?")
								}
							} else {
								result = append(result, findAffectorsInFunc(pass, stmt2.Rhs[i], within)...)
							}
						}
					case *ast.SelectorExpr:
						logf("findAffectorsInFunc is looking at an assignment inside a value of interest?  fun\n")
					}
				}
			}
			return true
		})
	case *ast.UnaryExpr:
		// This might be creating a pointer, which might fulfill the error interface.  If so, we're done (and it's important to remember the pointerness).
		if types.Implements(pass.TypesInfo.Types[expr].Type, tError) { // TODO the docs of this function are not truthfully admitting how specific this is.
			return []ast.Expr{expr}
		}
		return findAffectorsInFunc(pass, exprt.X, within)
	case *ast.CompositeLit, *ast.BasicLit: // Actual value creation!
		return []ast.Expr{expr}
	default:
		logf(":: findAffectorsInFunc does not yet handle %#v\n", expr)
	}
	return
}

func findAffectorsOfErrorReturnInFunc(pass *analysis.Pass, funcDecl *ast.FuncDecl) (affectors []ast.Expr, codes codeSet) {
	// TODO this should probably be approximately a good point for memoization?
	ast.Inspect(funcDecl, func(node ast.Node) bool {
		switch stmt := node.(type) {
		case *ast.FuncLit:
			return false // We don't want to see return statements from in a nested function right now.
		case *ast.ReturnStmt:
			// TODO stmt.Results can also be nil, in which case you have to look back at vars in the func sig.
			logf("function %q has a return statement: %s\n", funcDecl.Name.Name, stmt.Results)
			// This can go a lot of ways:
			// - You can have a plain `*ast.Ident` (aka returning a variable).
			// - You can have an `*ast.SelectorExpr` (returning a variable from in a structure).
			// - You can have an `*ast.CallExpr` (aka returning the result of a function call).
			// - You can have an `*ast.UnaryExpr` (probably about to be an '&' and then a structure literal, but could be other things too...).
			// - This is probably not an exhaustive list...

			lastResult := stmt.Results[len(stmt.Results)-1]
			newAffectors, newCodes := findAffectors(pass, lastResult, funcDecl)
			affectors = append(affectors, newAffectors...)
			codes = union(codes, newCodes)
		}
		return true
	})
	return
}

// findAffectors applies findAffectorsInFunc, and then _keeps going_...
// until it's resolved everything into one of:
//  - value creation,
//  - a CallExpr that targets another function that has declared error codes (yay!),
//  - a CallExpr that crosses package boundaries,
//  - a CallExpr that's an interface (we can't really look deeper than that),
//  - a CallExpr it's seen before,
//  - ... I think that's it?
//
// For the first two: we're happy: we can analyse this func completely.
// Encountering any of the others means we've found a source of unknowns.
func findAffectors(pass *analysis.Pass, expr ast.Expr, startingFunc *ast.FuncDecl) (affectors []ast.Expr, codes codeSet) {
	stepResults := findAffectorsInFunc(pass, expr, startingFunc)
	for _, x := range stepResults {
		switch exprt := x.(type) {
		case *ast.CallExpr:
			// For a CallExpr we first look if the error codes are already computed and stored as a fact.
			// If so we use those, otherwise we try to recurse and compute error codes for that function.
			callee := typeutil.Callee(pass.TypesInfo, exprt)
			var fact ErrorCodes
			if pass.ImportObjectFact(callee, &fact) {
				codes = union(codes, sliceToSet(fact.Codes))
			} else {
				switch funst := exprt.Fun.(type) {
				case *ast.Ident: // this is what calls in your own package look like. // TODO and dot-imported, I guess.  Yeesh.
					calledFunc := funst.Obj.Decl.(*ast.FuncDecl)
					newAffectors, newCodes := findAffectorsOfErrorReturnInFunc(pass, calledFunc)
					affectors = append(affectors, newAffectors...)
					codes = union(codes, newCodes)
				case *ast.SelectorExpr: // this is what calls to other packages look like. (but can also be method call on a type)
					logf("todo: findAffectors doesn't yet search beyond selector expressions %#v\n", funst)
				}
			}
		case *ast.CompositeLit, *ast.BasicLit:
			affectors = append(affectors, x)
		default:
			affectors = append(affectors, x)
		}
	}
	return
}

// checkErrorTypeHasLegibleCode makes sure that the `Code() string` function
// on a type either returns a constant or a single struct field.
// If you want to write your own ree.Error, it should be this simple.
func checkErrorTypeHasLegibleCode(pass *analysis.Pass, seen ast.Expr) bool { // probably should return a lookup function.
	typ := pass.TypesInfo.Types[seen].Type
	return types.Implements(typ, tReeError)
}

// extractErrorCodes inspects the error type.
//
// If the Code() method returns a constant value: That is the error code we're looking for
// If the Code() method returns a single struct field:
//     Store the field position and identifier
//         Position needed for tracking creation with a constructor
//         Identifier needed for tracking direct assignments to the field
//     Inspect the function where this error is created and find all possible error codes
//     (We only ever consider constant value expressions. Everything else would be hard to impossible to track.)
func extractErrorCodes(pass *analysis.Pass, expr ast.Expr, funcDecl *ast.FuncDecl) codeSet {
	result := codeSet{}
	switch expr := expr.(type) {
	case *ast.CompositeLit:
		// TODO: Implement (This is only a dummy implementation so far!)
		if len(expr.Elts) == 1 {
			if info, ok := pass.TypesInfo.Types[expr.Elts[0]]; ok && info.Value != nil {
				if info.Value.Kind() == constant.String {
					value := info.Value.String()
					value, err := strconv.Unquote(value)
					if err == nil && isErrorCodeValid(value) {
						result.add(value)
					} else {
						pass.ReportRangef(expr, "error code from expression has invalid format: should match [a-zA-Z][a-zA-Z0-9\\-]*[a-zA-Z0-9]")
					}
				}
			}
		}
	case *ast.UnaryExpr:
		if expr.Op == token.AND {
			result = union(result, extractErrorCodes(pass, expr.X, funcDecl))
		}
	default:
		logf("extractErrorCodes did not yet handle: %#v\n", expr)
	}
	return result
}

// TODO: Store the result per errorType. The problem is: equal types don't seem to be equal (see errorTypesEqual())
//       Possible solution: Export the information as a fact. That should also allow the usage of errors of other packages.
func analyseErrorType(pass *analysis.Pass, errorType types.Type) (codeSet, []string) {
	funcDecl, receiver := getCodeFuncFromErrorType(pass, errorType)
	if funcDecl == nil {
		return nil, nil
	}

	// Looking through return statements of the "Code() string" method
	// in order to find error code constants or
	// the return of a single field.
	// For all other return statements we mark it as invalid by emitting a diagnostic.
	constants := set()
	ast.Inspect(funcDecl, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false // Were not interested in return statements of nested function literals
		case *ast.ReturnStmt:
			if node.Results == nil || len(node.Results) != 1 {
				panic("Should be unreachable: We already know that the method returns a single value. Return statements that don't do so should lead to a compile time error.")
			}

			// If the return statement returns a constant string value:
			// Check if it is a valid error code and if so add it to the error code constants.
			returnType := pass.TypesInfo.Types[node.Results[0]]
			if value, ok := stringFromConstant(returnType.Value); ok {
				if isErrorCodeValid(value) {
					constants.add(value)
				} else {
					pass.ReportRangef(node, "error code from expression has invalid format: should match [a-zA-Z][a-zA-Z0-9\\-]*[a-zA-Z0-9]")
				}
				return false
			}

			// TODO: Should we dissalow assignment to the error code field inside of the "Code" function? What about other possible modifications in methods of the error?
			expression, ok := node.Results[0].(*ast.SelectorExpr)
			if ok {
				if ident, ok := expression.X.(*ast.Ident); ok && ident.Obj == receiver.Obj {
					logf("Found single field: %q\n", expression.Sel.Name)
					return false
				}
			}

			pass.ReportRangef(node, "function %q should always return a string constant or a single field", funcDecl.Name.Name)
		}
		return true
	})

	return constants, nil
}

// getCodeFuncFromErrorType finds and returns the method declaration of "Code() string" for the given error type.
// The second result is the identifier which is the receiver of the method.
func getCodeFuncFromErrorType(pass *analysis.Pass, errorType types.Type) (result *ast.FuncDecl, receiver *ast.Ident) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	nodeFilter := []ast.Node{
		(*ast.FuncDecl)(nil),
	}

	// Search through all function declarations,
	// to find the "Code() string" method of the given error type.
	// Every branch exits with "return false" because we don't want too look into the function body here.
	inspect.Nodes(nodeFilter, func(node ast.Node, _ bool) bool {
		funcDecl := node.(*ast.FuncDecl)
		if funcDecl.Recv == nil || funcDecl.Recv.List == nil ||
			len(funcDecl.Recv.List) != 1 || funcDecl.Name.Name != "Code" {
			return false
		}

		receiverField := funcDecl.Recv.List[0]
		if !errorTypesSubset(pass.TypesInfo.Types[receiverField.Type].Type, errorType) ||
			len(receiverField.Names) != 1 {
			return false
		}

		result = funcDecl
		receiver = receiverField.Names[0]
		return false
	})

	return
}

// errorTypesSubset checks if type1 is a subset of type2.
func errorTypesSubset(type1, type2 types.Type) bool {
	if type1 == type2 {
		return true
	}

	pointer1, ok1 := type1.(*types.Pointer)
	pointer2, ok2 := type2.(*types.Pointer)
	return (ok1 && ok2 && pointer1.Elem() == pointer2.Elem()) ||
		(!ok1 && ok2 && type1 == pointer2.Elem())
}

// stringFromConstant tries to get concrete string value of the given constant value.
func stringFromConstant(value constant.Value) (string, bool) {
	if value != nil && value.Kind() == constant.String {
		value := value.String()
		value, err := strconv.Unquote(value)
		if err == nil {
			return value, true
		}
	}
	return "", false
}
