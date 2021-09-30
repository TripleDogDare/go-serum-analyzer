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

	"github.com/warpfork/go-ree/analysis/scc"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/types/typeutil"
)

var logf = fmt.Printf

// var logf = func(_ string, _ ...interface{}) {}

var VerifyAnalyzer = &analysis.Analyzer{
	Name:     "reeverify",
	Doc:      "Checks that any function that has a ree-style docstring enumerating error codes is telling the truth.",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      runVerify,
	FactTypes: []analysis.Fact{
		new(ErrorCodes),
		new(ErrorType),
	},
}

type ErrorCodes struct {
	Codes []string
}

func (*ErrorCodes) AFact() {}

func (e *ErrorCodes) String() string {
	sort.Strings(e.Codes)
	return fmt.Sprintf("ErrorCodes: %v", strings.Join(e.Codes, " "))
}

// ErrorType is a fact about a ree.Error type,
// declaring which error codes Code() might return,
// and/or what field gets returned by a call to Code().
type ErrorType struct {
	Codes []string        // error codes, or nil
	Field *ErrorCodeField // field information, or nil
}

// ErrorCodeField is part of ErrorType,
// and declares the field that might be returned by the Code() method of the ree.Error.
type ErrorCodeField struct {
	Name     string
	Position int
}

func (*ErrorType) AFact() {}

func (e *ErrorType) String() string {
	sort.Strings(e.Codes)
	return fmt.Sprintf("ErrorType{Field:%v, Codes:%v}", e.Field, strings.Join(e.Codes, " "))
}

func (f *ErrorCodeField) String() string {
	return fmt.Sprintf("{Name:%q, Position:%d}", f.Name, f.Position)
}

type funcCodes map[*ast.FuncDecl]codeSet

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

// isMethod checks if funcDecl is a method by looking if it has a single receiver.
func isMethod(funcDecl *ast.FuncDecl) bool {
	return funcDecl != nil && funcDecl.Recv != nil && len(funcDecl.Recv.List) == 1
}

func runVerify(pass *analysis.Pass) (interface{}, error) {
	lookup := collectFunctions(pass)

	findAndTagErrorTypes(pass, lookup)

	funcsToAnalyse := findErrorReturningFunctions(pass, lookup)

	// Out of funcsToAnalyse get all functions that declare error codes and the actual codes they declare.
	// In the remaining analysis we only look at the functions that declare error codes or get called by an analysed function.
	funcClaims := findClaimedErrorCodes(pass, funcsToAnalyse)

	// Okay -- let's look at the functions that have made claims about their error codes.
	// We'll explore deeply to find everything that can actually affect their error return value.
	// When we reach data initialization... we look at if those types implement coded errors, and try to figure out what their actual code value is.
	// When we reach other function calls that declare their errors, that's good enough info (assuming they're also being checked for truthfulness).
	// Anything else is trouble.
	scc := scc.StartSCC() // SCC for handling of recursive functions
	for funcDecl, claimedCodes := range funcClaims {
		analysisResult, ok := lookup.analysisResults[funcDecl]
		if !ok {
			analysisResult = findAffectorsOfErrorReturnInFunc(pass, lookup, scc, funcDecl)
		}
		affectorCodes := extractErrorCodesFromAffectors(pass, lookup, funcDecl, analysisResult.affectors)
		foundCodes := union(analysisResult.codes, affectorCodes)

		reportIfCodesDoNotMatch(pass, funcDecl, foundCodes, claimedCodes)
	}

	// Export all claimed error codes as facts.
	// Missing error code docs or unused ones will get reported in the respective functions,
	// but on caller site only the documented behaviour matters.
	exportFunctionErrorCodes(pass, funcClaims)

	return nil, nil
}

var tError = types.NewInterfaceType([]*types.Func{
	types.NewFunc(token.NoPos, nil, "Error", types.NewSignature(nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "", types.Typ[types.String])), false)),
}, nil).Complete()

var tReeError = types.NewInterfaceType([]*types.Func{
	types.NewFunc(token.NoPos, nil, "Error", types.NewSignature(nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "", types.Typ[types.String])), false)),
	types.NewFunc(token.NoPos, nil, "Code", types.NewSignature(nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "", types.Typ[types.String])), false)),
}, nil).Complete()

var tReeErrorWithCause = types.NewInterfaceType([]*types.Func{
	tReeError.Method(0),
	tReeError.Method(1),
	types.NewFunc(token.NoPos, nil, "Cause", types.NewSignature(nil, nil, types.NewTuple(types.NewVar(token.NoPos, nil, "", types.NewNamed(types.NewTypeName(token.NoPos, nil, "error", tError), nil, nil))), false)),
}, nil).Complete()

// findErrorReturningFunctions looks for functions that return an error,
// and emits a diagnostic if a function returns an error, but not as the last argument.
func findErrorReturningFunctions(pass *analysis.Pass, lookup *funcLookup) []*ast.FuncDecl {
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
	lookup.forEach(func(funcDecl *ast.FuncDecl) {
		resultsList := funcDecl.Type.Results
		if resultsList == nil {
			return
		}
		lastResult := resultsList.List[len(resultsList.List)-1]
		typ := pass.TypesInfo.TypeOf(lastResult.Type)
		if !types.Implements(typ, tError) {
			// Emit diagnostic if an error is returned as non-last argument
			for _, result := range resultsList.List {
				typ := pass.TypesInfo.TypeOf(result.Type)
				if types.Implements(typ, tError) {
					pass.Reportf(result.Pos(), "error should be returned as the last argument")
				}
			}
			return
		}
		funcsToAnalyse = append(funcsToAnalyse, funcDecl)
	})

	return funcsToAnalyse
}

// findClaimedErrorCodes finds the error codes claimed by the given functions,
// and emits diagnostics if a function does not claim error codes or
// if the format of the docstring does not match the expected format.
func findClaimedErrorCodes(pass *analysis.Pass, funcsToAnalyse []*ast.FuncDecl) funcCodes {
	result := funcCodes{}
	for _, funcDecl := range funcsToAnalyse {
		codes, err := findErrorDocs(funcDecl)
		if err != nil {
			pass.Reportf(funcDecl.Pos(), "function %q has odd docstring: %s", funcDecl.Name.Name, err)
			continue
		}

		if len(codes) == 0 {
			// Exclude Cause() methods of error types from having to declare error codes.
			// If a Cause() method declares error codes, treat it like every other method.
			if isMethod(funcDecl) {
				receiverType := pass.TypesInfo.TypeOf(funcDecl.Recv.List[0].Type)
				if types.Implements(receiverType, tReeErrorWithCause) {
					continue
				}
			}

			// Warn directly about any functions that are exported if they return errors,
			// but don't declare error codes in their docs.
			if funcDecl.Name.IsExported() {
				pass.Reportf(funcDecl.Pos(), "function %q is exported, but does not declare any error codes", funcDecl.Name.Name)
			}
		} else {
			result[funcDecl] = codes
		}
	}

	return result
}

func findErrorDocs(funcDecl *ast.FuncDecl) (codeSet, error) {
	if funcDecl.Doc == nil {
		return nil, nil
	}
	return findErrorDocsSM{}.run(funcDecl.Doc.Text())
}

// exportFunctionErrorCodes exports all codes for each function in the given map as facts.
func exportFunctionErrorCodes(pass *analysis.Pass, codes funcCodes) {
	for funcDecl, codeSet := range codes {
		fn, ok := pass.TypesInfo.Defs[funcDecl.Name].(*types.Func)
		if !ok {
			logf("Could not find definition for function %q!", funcDecl.Name.Name)
			continue
		}

		fact := ErrorCodes{codeSet.slice()}
		pass.ExportObjectFact(fn, &fact)
	}
}

// extractErrorCodesFromAffectors extracts all error codes from the given affectors and returns them.
func extractErrorCodesFromAffectors(pass *analysis.Pass, lookup *funcLookup, funcDecl *ast.FuncDecl, affectors map[ast.Expr]struct{}) codeSet {
	result := set()
	for affector := range affectors {
		// Make sure method "Code() string" is present
		if !checkErrorTypeHasLegibleCode(pass, affector) {
			pass.ReportRangef(affector, "expression does not define an error code")
			continue
		}

		errorType, err := getErrorTypeForError(pass, lookup, pass.TypesInfo.Types[affector].Type)
		if err != nil || errorType == nil {
			pass.ReportRangef(affector, "expression is not a valid error: error types must return constant error codes or a single field")
		}
		if err != nil {
			logf("Error while looking at affector: %v (Affector: %#v)\n", err, affector)
		} else if errorType != nil {
			if len(errorType.Codes) > 0 {
				result = unionInplace(result, sliceToSet(errorType.Codes))
			}

			if errorType.Field != nil {
				code, err := extractFieldErrorCodes(pass, affector, funcDecl, errorType)
				if err == nil {
					result.add(code)
				} else {
					pass.ReportRangef(affector, "%v", err)
				}
			}
		}
	}

	return result
}

// reportIfCodesDoNotMatch emits a diagnostic if the given code collections don't match.
func reportIfCodesDoNotMatch(pass *analysis.Pass, funcDecl *ast.FuncDecl, foundCodes codeSet, claimedCodes codeSet) {
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

	if len(errorMessages) != 0 {
		errorMessage := strings.Join(errorMessages, " ")
		pass.Reportf(funcDecl.Pos(), "function %q has a mismatch of declared and actual error codes: %s", funcDecl.Name.Name, errorMessage)
	}
}

// findAffectorsInFunc looks up what can affect the given expression
// (which, generally, can be anything you'd expect to see in a ReturnStmt -- so, variables, unaryExpr, a bunch of things...),
// and recurses in this until it hits either the creation of a value,
// or function call boundaries (`*ast.CallExpr`).
//
// So, it'll follow any number of assignment statements, for example;
// as it does so, it'll totally disregarding logical branching,
// instead using a very basic model of taint: just marking anything that can ever possibly touch the variable.
func findAffectorsInFunc(pass *analysis.Pass, expr ast.Expr, within *ast.FuncDecl, visited map[*ast.Ident]struct{}) (result []ast.Expr) {
	switch exprt := expr.(type) {
	case *ast.CallExpr: // These are a boundary condition, so that's short and sweet.
		return []ast.Expr{expr}
	case *ast.Ident: // Lovely!  These are easy.  (Although likely to have significant taint spread.)
		// Mark ident as visited to avoid revisiting it again (possibly resulting in an endles loop)
		if _, ok := visited[exprt]; ok {
			return
		}
		visited[exprt] = struct{}{}

		// Check that the identifier is a local variable.
		if exprt.Obj != nil {
			withinPos := within.Body.Pos()
			if within.Type.Results != nil {
				// Results are allowed too, because named results may be declared there.
				withinPos = within.Type.Results.Pos()
			}

			if exprt.Obj.Pos() <= withinPos || exprt.Obj.Pos() >= within.Body.End() {
				pass.ReportRangef(exprt, "returned error may not be a parameter, receiver or global variable")
			}
		}

		// Look for for `*ast.AssignStmt` in the function that could've affected this.
		ast.Inspect(within, func(node ast.Node) bool {
			// n.b., do *not* filter out *`ast.FuncLit`: statements inside closures can assign things!
			assignment, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}

			// Look for our ident's object in the left-hand-side of the assign.
			// Either follow up on the statement at the same index in the Rhs,
			// or watch out for a shorter Rhs that's just a CallExpr (i.e. it's a destructuring assignment).
			for i, clause := range assignment.Lhs {
				switch clauset := clause.(type) {
				case *ast.Ident:
					if clauset.Obj == exprt.Obj {
						if len(assignment.Lhs) > len(assignment.Rhs) {
							// Destructuring mode.
							// We're going to make some crass simplifications here, and say... if this is anything other than the last arg, you're not supported.
							if i != len(assignment.Lhs)-1 {
								pass.ReportRangef(clauset, "unsupported: tracking error codes for function call with error as non-last return argument")
								return false
							}
							// Because it's a CallExpr, we're done here: this is part of the result.
							if callExpr, ok := assignment.Rhs[0].(*ast.CallExpr); ok {
								result = append(result, callExpr)
							} else {
								panic("what?")
							}
						} else {
							result = append(result, findAffectorsInFunc(pass, assignment.Rhs[i], within, visited)...)
						}
					}
				case *ast.SelectorExpr:
					// TODO: Implement assignment to fields of errors
				}
			}
			return true
		})
	case *ast.UnaryExpr:
		// This might be creating a pointer, which might fulfill the error interface.  If so, we're done (and it's important to remember the pointerness).
		if exprt.Op == token.AND && types.Implements(pass.TypesInfo.TypeOf(expr), tError) { // TODO the docs of this function are not truthfully admitting how specific this is.
			return []ast.Expr{expr}
		}

		// If it's not fulfilling the error interface it's not supported
		pass.ReportRangef(exprt, "expression does not implement valid error type")
		return nil
	case *ast.CompositeLit, *ast.BasicLit: // Actual value creation!
		return []ast.Expr{expr}
	default:
		logf(":: findAffectorsInFunc does not yet handle %#v\n", expr)
	}
	return
}

func findAffectorsOfErrorReturnInFunc(pass *analysis.Pass, lookup *funcLookup, scc scc.State, funcDecl *ast.FuncDecl) funcAnalysisResult {
	scc.Visit(funcDecl)
	result := funcAnalysisResult{}

	ast.Inspect(funcDecl, func(node ast.Node) bool {
		switch stmt := node.(type) {
		case *ast.FuncLit:
			return false // We don't want to see return statements from in a nested function right now.
		case *ast.ReturnStmt:
			// stmt.Results can also be nil, in which case you have to look back at vars in the func sig.
			var resultExpression ast.Expr
			if len(stmt.Results) == 0 {
				resultTypes := funcDecl.Type.Results.List
				if len(resultTypes) == 0 {
					panic("Should be unreachable: we already know that the function signature contains an error result.")
				}

				resultIdents := resultTypes[len(resultTypes)-1].Names
				if len(resultIdents) == 0 {
					panic("Should be unreachable: an empty return statement requires either empty result list or named results.")
				}

				resultExpression = resultIdents[len(resultIdents)-1]
			} else {
				resultExpression = stmt.Results[len(stmt.Results)-1]
			}

			// This can go a lot of ways:
			// - You can have a plain `*ast.Ident` (aka returning a variable).
			// - You can have an `*ast.SelectorExpr` (returning a variable from in a structure).
			// - You can have an `*ast.CallExpr` (aka returning the result of a function call).
			// - You can have an `*ast.UnaryExpr` (probably about to be an '&' and then a structure literal, but could be other things too...).
			// - This is probably not an exhaustive list...
			if resultExpression != nil {
				analysisResult := findAffectors(pass, lookup, scc, resultExpression, funcDecl)
				result.combineInplace(analysisResult)
			}

			return false
		}
		return true
	})

	lookup.analysisResults[funcDecl] = result

	isComponentRoot, component := scc.EndVisit(funcDecl)
	if isComponentRoot {
		return unifyAnalysisResultForComponent(lookup, component)
	}

	return result
}

// unifyAnalysisResultForComponent sets the analysis result of each function in the given component to a combined result,
// containing all the error codes and affectors that result from the analysis of the individual functions.
func unifyAnalysisResultForComponent(lookup *funcLookup, component scc.Component) funcAnalysisResult {
	var result funcAnalysisResult

	// Create unified result using all individual results of the functions in the component.
	for _, element := range component {
		funcDecl := element.(*ast.FuncDecl)
		analysisResult := lookup.analysisResults[funcDecl]

		// lookup.analysisResults[funcDecl] will be overwritten in the next step, so using combineInplace is fine.
		result.combineInplace(analysisResult)
	}

	// Set the unified result to all functions in the component.
	for _, element := range component {
		funcDecl := element.(*ast.FuncDecl)
		lookup.analysisResults[funcDecl] = result
	}

	return result
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
func findAffectors(pass *analysis.Pass, lookup *funcLookup, scc scc.State, expr ast.Expr, startingFunc *ast.FuncDecl) funcAnalysisResult {
	var result funcAnalysisResult
	stepResults := findAffectorsInFunc(pass, expr, startingFunc, map[*ast.Ident]struct{}{})
	for _, x := range stepResults {
		switch exprt := x.(type) {
		case *ast.CallExpr:
			// For a CallExpr we first look if the error codes are already computed and stored as a fact.
			// If so we use those, otherwise we try to recurse and compute error codes for that function.
			callee := typeutil.Callee(pass.TypesInfo, exprt)
			var fact ErrorCodes
			if callee != nil && pass.ImportObjectFact(callee, &fact) {
				result.codes = unionInplace(result.codes, sliceToSet(fact.Codes))
			} else {
				var calledFunc *ast.FuncDecl

				switch funst := exprt.Fun.(type) {
				case *ast.Ident: // this is what calls in your own package look like. // TODO and dot-imported, I guess.  Yeesh.
					switch funcDecl := funst.Obj.Decl.(type) {
					case *ast.FuncDecl: // Noramal function call
						calledFunc = funcDecl
					case *ast.TypeSpec: // Type conversion
						result.addAffector(exprt)
						continue
					}
				case *ast.SelectorExpr: // this is what calls to other packages look like. (but can also be method call on a type)
					if target, ok := funst.X.(*ast.Ident); ok {
						if obj, ok := pass.TypesInfo.ObjectOf(target).(*types.PkgName); ok {
							// We're calling a function in a package that does not have declared error codes
							pass.ReportRangef(funst, "function %q in package %q does not declare error codes", funst.Sel.Name, obj.Imported().Name())
							continue
						}
					}

					// This case is gonna be harder than functions: We need to figure out which function declaration applies,
					// because there is no object information provided for methods calls.
					selection := pass.TypesInfo.Selections[funst]
					calledFunc = lookup.searchMethod(pass, selection.Recv(), funst.Sel.Name)
				default:
					panic("Fun of an ast.CallExpr which is neither an ast.Ident nor an ast.SelectorExpr")
				}

				if calledFunc != nil {
					var analysisResult funcAnalysisResult

					shouldRecurse := scc.HandleEdge(startingFunc, calledFunc)
					if shouldRecurse {
						analysisResult = findAffectorsOfErrorReturnInFunc(pass, lookup, scc, calledFunc)
						scc.AfterRecurse(startingFunc, calledFunc)
					} else if cachedResult, ok := lookup.analysisResults[calledFunc]; ok {
						analysisResult = cachedResult
					}

					// combine can handle nil values.
					result = result.combine(analysisResult)
				} else {
					// Could e.g. be a method which is defined in another package
					pass.ReportRangef(exprt.Fun, "called function does not declare error codes")
				}
			}
		case *ast.CompositeLit, *ast.BasicLit:
			result.addAffector(x)
		default:
			result.addAffector(x)
		}
	}

	return result
}

// checkErrorTypeHasLegibleCode makes sure that the `Code() string` function
// on a type either returns a constant or a single struct field.
// If you want to write your own ree.Error, it should be this simple.
func checkErrorTypeHasLegibleCode(pass *analysis.Pass, seen ast.Expr) bool { // probably should return a lookup function.
	typ := pass.TypesInfo.TypeOf(seen)
	return types.Implements(typ, tReeError)
}

// extractFieldErrorCodes finds a possible error code from the given constructor expression.
//
// The expression evaluates to an error of the given error type, which has its errorType.Field set to a value (not nil).
func extractFieldErrorCodes(pass *analysis.Pass, expr ast.Expr, funcDecl *ast.FuncDecl, errorType *ErrorType) (string, error) {
	if errorType == nil || errorType.Field == nil {
		return "", fmt.Errorf("cannot extract field error code without field definition")
	}

	switch expr := expr.(type) {
	case *ast.CompositeLit:
		// Key-based composite literal:
		// Use the field name to find the error code.
		for _, element := range expr.Elts {
			element, ok := element.(*ast.KeyValueExpr)
			if !ok { // Either all elements are KeyValueExpr or none.
				break
			}

			ident, ok := element.Key.(*ast.Ident)
			if !ok {
				logf("found weird key %#v in composite literal %#v\n", element.Key, expr)
				break
			}

			if errorType.Field.Name == ident.Name {
				info, ok := pass.TypesInfo.Types[element.Value]
				if ok && info.Value != nil {
					return getErrorCodeFromConstant(info.Value)
				}
			}
		}

		// Position-based composite literal:
		// Use the field position to find the error code.
		pos := errorType.Field.Position
		if pos < len(expr.Elts) {
			info, ok := pass.TypesInfo.Types[expr.Elts[pos]]
			if ok && info.Value != nil {
				return getErrorCodeFromConstant(info.Value)
			}
		}
	case *ast.UnaryExpr:
		if expr.Op == token.AND {
			return extractFieldErrorCodes(pass, expr.X, funcDecl, errorType)
		}
	default:
		logf("extractErrorCodes did not yet handle: %#v\n", expr)
	}

	return "", fmt.Errorf("error code field has to be instantiated by constant value")
}

func getErrorCodeFromConstant(value constant.Value) (string, error) {
	if value.Kind() != constant.String {
		// Should not be reachable, because we already checked the signature of Code() to return a string.
		// And the value is in the end one that gets returned by Code().
		// So there should be a compiler error if value is not of type string.
		return "", fmt.Errorf("error code has to be of type string")
	}

	result := value.String()
	result, err := strconv.Unquote(result)
	if err != nil {
		return "", fmt.Errorf("problem unquoting string constant value: %v", err)
	}

	if !isErrorCodeValid(result) {
		return "", fmt.Errorf("error code has invalid format: should match [a-zA-Z][a-zA-Z0-9\\-]*[a-zA-Z0-9]")
	}

	return result, nil
}
