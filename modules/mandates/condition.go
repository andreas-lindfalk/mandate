package mandates

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"

	"github.com/andreas-lindfalk/pgmandate/pkg/fault"
)

// A condition narrows what a mandate allows by the content of a proposal,
// such as payload.amount <= 500 && payload.currency in ["EUR", "SEK"]. It is
// written in CEL and limited to what the database can check as well:
// payload fields and the action key compared with literal values, combined
// with &&, || and !, and has() for a field that is present. The service
// compiles it once, at the grant, to a Plan; the service and the database
// then evaluate the same plan, so the person granting writes one condition
// and never a second form of it.
//
// The plan's meaning, which the two evaluators share:
//   - Numbers compare exactly, as decimals: 0.1 + 0.2 is not a question here,
//     and 500 equals 500.0.
//   - Strings compare by their bytes, the order of the "C" collation.
//   - Booleans compare only with == and !=.
//   - A missing field, a field of another type than the literal, and anything
//     else that cannot be evaluated is unknown. Unknown never allows: && and ||
//     treat it as SQL treats NULL, ! keeps it unknown, and only a condition
//     that is true lets a mandate allow the proposal.
const (
	maxConditionLength = 2000
	maxConditionNodes  = 100
	maxPathLength      = 10
	maxInValues        = 100
	maxKeyLength       = 200
)

// Plan is a compiled condition: what the service and the database
// evaluate. It is stored with the mandate beside the condition's text.
type Plan struct {
	// Op is and, or, not, ==, !=, <, <=, >, >=, in or has.
	Op string `json:"op"`
	// Args are the operands of and, or and not.
	Args []Plan `json:"args,omitempty"`
	// Path names a value: ["action"], or ["payload", "field", ...].
	Path []string `json:"path,omitempty"`
	// Value is the literal a comparison compares with.
	Value json.RawMessage `json:"value,omitempty"`
	// Values are the literals of in.
	Values []json.RawMessage `json:"values,omitempty"`
}

const (
	varPayload = "payload"
	varAction  = "action"
)

var conditionEnv = sync.OnceValues(func() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable(varPayload, cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable(varAction, cel.StringType),
		cel.ParserRecursionLimit(32),
		cel.ParserExpressionSizeLimit(maxConditionLength),
	)
})

const conditionHelp = "A condition compares payload fields or the action with literal values (== != < <= > >= and in [...]), " +
	"combines comparisons with && || and !, and tests a field with has(payload.field)."

// CompileCondition parses and checks a condition and returns its plan. The
// error is an InvalidArgument that says what is wrong and where.
func CompileCondition(source string) (Plan, error) {
	if utf8.RuneCountInString(source) > maxConditionLength {
		return Plan{}, conditionError("the condition can be at most %d characters", maxConditionLength)
	}
	if strings.TrimSpace(source) == "" {
		return Plan{}, conditionError("the condition is empty")
	}
	if strings.ContainsRune(source, 0) {
		return Plan{}, conditionError("the condition cannot contain a NUL character")
	}
	env, err := conditionEnv()
	if err != nil {
		return Plan{}, err
	}
	checked, issues := env.Compile(source)
	if issues != nil && issues.Err() != nil {
		return Plan{}, conditionError("the condition is not valid CEL: %s", strings.TrimSpace(issues.String()))
	}
	if !checked.OutputType().IsExactType(cel.BoolType) {
		return Plan{}, conditionError("the condition must be true or false, not %s: compare the field, as in payload.flag == true", checked.OutputType())
	}
	native := checked.NativeRep()
	c := compiler{info: native.SourceInfo()}
	return c.condition(native.Expr())
}

func conditionError(format string, args ...any) error {
	return fault.New(fault.InvalidArgument, fmt.Sprintf(format, args...))
}

type compiler struct {
	info  *celast.SourceInfo
	nodes int
}

// unsupported names the part of the condition that falls outside the
// subset, by its position.
func (c *compiler) unsupported(e celast.Expr, what string) error {
	at := c.info.GetStartLocation(e.ID())
	return conditionError("%s (line %d, column %d) is not supported yet. %s", what, at.Line(), at.Column()+1, conditionHelp)
}

func (c *compiler) condition(e celast.Expr) (Plan, error) {
	c.nodes++
	if c.nodes > maxConditionNodes {
		return Plan{}, conditionError("the condition has more than %d parts", maxConditionNodes)
	}
	switch e.Kind() {
	case celast.CallKind:
		call := e.AsCall()
		if call.IsMemberFunction() {
			return Plan{}, c.unsupported(e, "the function "+call.FunctionName())
		}
		switch name := call.FunctionName(); name {
		case operators.LogicalAnd, operators.LogicalOr:
			op := "and"
			if name == operators.LogicalOr {
				op = "or"
			}
			plan := Plan{Op: op}
			for _, arg := range call.Args() {
				sub, err := c.condition(arg)
				if err != nil {
					return Plan{}, err
				}
				plan.Args = append(plan.Args, sub)
			}
			return plan, nil
		case operators.LogicalNot:
			sub, err := c.condition(call.Args()[0])
			if err != nil {
				return Plan{}, err
			}
			return Plan{Op: "not", Args: []Plan{sub}}, nil
		case operators.Equals, operators.NotEquals, operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals:
			return c.compare(e, name, call.Args())
		case operators.In:
			return c.in(e, call.Args())
		}
		return Plan{}, c.unsupported(e, "the function "+call.FunctionName())
	case celast.SelectKind:
		sel := e.AsSelect()
		if sel.IsTestOnly() {
			path, err := c.steps(sel.Operand())
			if err != nil {
				return Plan{}, err
			}
			if path[0] != varPayload {
				return Plan{}, c.unsupported(e, "has() on anything but a payload field")
			}
			path = append(path, sel.FieldName())
			if err := checkPath(path); err != nil {
				return Plan{}, err
			}
			return Plan{Op: "has", Path: path}, nil
		}
	}
	return Plan{}, c.unsupported(e, "a value that is not a comparison")
}

var flipped = map[string]string{
	operators.Equals: "==", operators.NotEquals: "!=",
	operators.Less: ">", operators.LessEquals: ">=", operators.Greater: "<", operators.GreaterEquals: "<=",
}

var direct = map[string]string{
	operators.Equals: "==", operators.NotEquals: "!=",
	operators.Less: "<", operators.LessEquals: "<=", operators.Greater: ">", operators.GreaterEquals: ">=",
}

func (c *compiler) compare(e celast.Expr, name string, args []celast.Expr) (Plan, error) {
	var (
		op    string
		path  []string
		value json.RawMessage
		kind  string
		err   error
	)
	switch {
	case c.isPath(args[0]) && c.isPath(args[1]):
		return Plan{}, c.unsupported(e, "comparing two fields")
	case c.isPath(args[0]):
		op = direct[name]
		if path, err = c.path(args[0]); err != nil {
			return Plan{}, err
		}
		value, kind, err = c.literal(args[1])
	case c.isPath(args[1]):
		// A literal on the left: 500 >= payload.amount.
		op = flipped[name]
		if path, err = c.path(args[1]); err != nil {
			return Plan{}, err
		}
		value, kind, err = c.literal(args[0])
	default:
		return Plan{}, c.unsupported(e, "a comparison that is not between a field and a literal value")
	}
	if err != nil {
		return Plan{}, err
	}
	if kind == "boolean" && op != "==" && op != "!=" {
		return Plan{}, c.unsupported(e, "ordering true and false")
	}
	if path[0] == varAction && op != "==" && op != "!=" {
		return Plan{}, c.unsupported(e, "ordering action keys")
	}
	return Plan{Op: op, Path: path, Value: value}, nil
}

// isPath says whether an expression is a name or a field of one, as
// opposed to a literal or a call.
func (c *compiler) isPath(e celast.Expr) bool {
	switch e.Kind() {
	case celast.IdentKind:
		return true
	case celast.SelectKind:
		return !e.AsSelect().IsTestOnly()
	case celast.CallKind:
		return e.AsCall().FunctionName() == operators.Index
	}
	return false
}

func (c *compiler) in(e celast.Expr, args []celast.Expr) (Plan, error) {
	path, err := c.path(args[0])
	if err != nil {
		return Plan{}, err
	}
	if args[1].Kind() != celast.ListKind {
		return Plan{}, c.unsupported(args[1], "in with something other than a list of literal values")
	}
	elements := args[1].AsList().Elements()
	if len(elements) == 0 || len(elements) > maxInValues {
		return Plan{}, conditionError("a list in the condition holds 1 to %d values", maxInValues)
	}
	plan := Plan{Op: "in", Path: path}
	first := ""
	for _, element := range elements {
		value, kind, err := c.literal(element)
		if err != nil {
			return Plan{}, err
		}
		if first == "" {
			first = kind
		} else if kind != first {
			return Plan{}, c.unsupported(element, "a list that mixes types")
		}
		plan.Values = append(plan.Values, value)
	}
	return plan, nil
}

// path reads payload.a.b, payload["a-b"] or action.
func (c *compiler) path(e celast.Expr) ([]string, error) {
	path, err := c.steps(e)
	if err != nil {
		return nil, err
	}
	switch {
	case path[0] == varAction && len(path) > 1:
		return nil, c.unsupported(e, "a field of the action")
	case path[0] != varAction && path[0] != varPayload:
		return nil, c.unsupported(e, "the name "+path[0])
	case path[0] == varPayload && len(path) == 1:
		return nil, c.unsupported(e, "the payload as a whole")
	}
	return path, checkPath(path)
}

// steps reads the names of a path from its root.
func (c *compiler) steps(e celast.Expr) ([]string, error) {
	switch e.Kind() {
	case celast.IdentKind:
		return []string{e.AsIdent()}, nil
	case celast.SelectKind:
		sel := e.AsSelect()
		if sel.IsTestOnly() {
			return nil, c.unsupported(e, "has() inside a comparison")
		}
		parent, err := c.steps(sel.Operand())
		if err != nil {
			return nil, err
		}
		return append(parent, sel.FieldName()), nil
	case celast.CallKind:
		call := e.AsCall()
		if call.FunctionName() != operators.Index || call.IsMemberFunction() {
			return nil, c.unsupported(e, "the function "+call.FunctionName())
		}
		parent, err := c.steps(call.Args()[0])
		if err != nil {
			return nil, err
		}
		key := call.Args()[1]
		if key.Kind() != celast.LiteralKind {
			return nil, c.unsupported(key, "an index that is not a literal string")
		}
		s, ok := key.AsLiteral().(types.String)
		if !ok {
			return nil, c.unsupported(key, "an index that is not a literal string")
		}
		return append(parent, string(s)), nil
	}
	return nil, c.unsupported(e, "a value that is not a field")
}

func checkPath(path []string) error {
	if len(path) > maxPathLength+1 {
		return conditionError("a field in the condition is at most %d levels deep", maxPathLength)
	}
	for _, key := range path[1:] {
		// The database's check of a plan takes no empty name either.
		if key == "" {
			return conditionError("a field name in the condition cannot be empty")
		}
		if len(key) > maxKeyLength {
			return conditionError("a field name in the condition is at most %d bytes", maxKeyLength)
		}
		if strings.ContainsRune(key, 0) {
			return conditionError("a field name in the condition cannot contain a NUL character")
		}
	}
	return nil
}

// literal reads a literal number, string or boolean as JSON, with the kind
// jsonb_typeof gives it.
func (c *compiler) literal(e celast.Expr) (json.RawMessage, string, error) {
	switch e.Kind() {
	case celast.LiteralKind:
		switch v := e.AsLiteral().(type) {
		case types.Int:
			return json.RawMessage(strconv.FormatInt(int64(v), 10)), "number", nil
		case types.Uint:
			return json.RawMessage(strconv.FormatUint(uint64(v), 10)), "number", nil
		case types.Double:
			f := float64(v)
			if math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, "", c.unsupported(e, "a number that is not finite")
			}
			// The shortest form that reads back as the same double: 0.1
			// stays 0.1 rather than its binary expansion.
			return json.RawMessage(strconv.FormatFloat(f, 'g', -1, 64)), "number", nil
		case types.String:
			// Postgres stores no NUL in text or jsonb, so no payload holds one.
			if strings.ContainsRune(string(v), 0) {
				return nil, "", c.unsupported(e, "a NUL character")
			}
			raw, err := json.Marshal(string(v))
			return raw, "string", err
		case types.Bool:
			return json.RawMessage(strconv.FormatBool(bool(v))), "boolean", nil
		case types.Null:
			return nil, "", c.unsupported(e, "comparing with null (has(payload.field) tells whether a field is present)")
		}
		return nil, "", c.unsupported(e, "a literal of type "+e.AsLiteral().Type().TypeName())
	case celast.CallKind:
		// -5 and -0.5 are a negation of a literal.
		call := e.AsCall()
		if call.FunctionName() == operators.Negate && !call.IsMemberFunction() {
			value, kind, err := c.literal(call.Args()[0])
			if err == nil && kind == "number" {
				if bytes.HasPrefix(value, []byte("-")) {
					return value[1:], kind, nil
				}
				return append(json.RawMessage("-"), value...), kind, nil
			}
		}
	}
	return nil, "", c.unsupported(e, "a value that is not a literal")
}

// Holds evaluates the plan on a proposal's action and payload. Only a
// condition that is true holds: false and unknown do not.
func (p Plan) Holds(action string, payload json.RawMessage) bool {
	value, known := p.Evaluate(action, payload)
	return known && value
}

// Evaluate is the three-valued evaluation that mandate.condition_holds in
// the database mirrors: known is false where the database returns NULL.
func (p Plan) Evaluate(action string, payload json.RawMessage) (value, known bool) {
	doc, ok := decodeJson(payload)
	if !ok {
		return false, false
	}
	return p.eval(action, doc)
}

// eval is the three-valued evaluation: known is false when the plan cannot
// say (a missing field, a type that does not match), as NULL in SQL.
func (p Plan) eval(action string, payload any) (value, known bool) {
	switch p.Op {
	case "and", "or":
		if len(p.Args) == 0 {
			return false, false
		}
		// false decides and, true decides or, whatever else is unknown.
		decisive := p.Op == "or"
		unknown := false
		for _, arg := range p.Args {
			v, k := arg.eval(action, payload)
			if !k {
				unknown = true
			} else if v == decisive {
				return decisive, true
			}
		}
		if unknown {
			return false, false
		}
		return !decisive, true
	case "not":
		if len(p.Args) != 1 {
			return false, false
		}
		v, k := p.Args[0].eval(action, payload)
		return !v, k
	case "==", "!=", "<", "<=", ">", ">=":
		left, found := lookup(p.Path, action, payload)
		right, ok := decodeJson(p.Value)
		if !found || !ok {
			return false, false
		}
		cmp, ok := compareJson(p.Op, left, right)
		if !ok {
			return false, false
		}
		switch p.Op {
		case "==":
			return cmp == 0, true
		case "!=":
			return cmp != 0, true
		case "<":
			return cmp < 0, true
		case "<=":
			return cmp <= 0, true
		case ">":
			return cmp > 0, true
		default:
			return cmp >= 0, true
		}
	case "in":
		left, found := lookup(p.Path, action, payload)
		if !found || len(p.Values) == 0 {
			return false, false
		}
		match := false
		for _, raw := range p.Values {
			right, ok := decodeJson(raw)
			if !ok || jsonKind(right) != jsonKind(left) {
				return false, false
			}
			// Values of a kind that does not compare (null, objects,
			// arrays) make the answer unknown, never a plain no.
			cmp, ok := compareJson("==", left, right)
			if !ok {
				return false, false
			}
			if cmp == 0 {
				match = true
			}
		}
		return match, true
	case "has":
		if len(p.Path) < 2 || p.Path[0] != varPayload {
			return false, false
		}
		// The parent of a top level field is the payload itself.
		parent, found := payload, true
		if len(p.Path) > 2 {
			parent, found = lookup(p.Path[:len(p.Path)-1], action, payload)
		}
		object, isObject := parent.(map[string]any)
		if !found || !isObject {
			return false, false
		}
		_, present := object[p.Path[len(p.Path)-1]]
		return present, true
	}
	return false, false
}

// lookup finds the value a path names. Every step below the payload must
// be an object: a key is never read as an array index.
func lookup(path []string, action string, payload any) (any, bool) {
	switch {
	case len(path) == 1 && path[0] == varAction:
		return action, true
	case len(path) < 2 || path[0] != varPayload:
		return nil, false
	}
	current := payload
	for _, key := range path[1:] {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[key]; !ok {
			return nil, false
		}
	}
	return current, true
}

// jsonKind names a decoded JSON value as jsonb_typeof does.
func jsonKind(v any) string {
	switch v.(type) {
	case json.Number:
		return "number"
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	}
	return "unknown"
}

// compareJson orders two values of the same kind; ok is false when they
// cannot be compared with the operator.
func compareJson(op string, left, right any) (int, bool) {
	kind := jsonKind(left)
	if kind != jsonKind(right) {
		return 0, false
	}
	switch kind {
	case "number":
		return compareDecimals(string(left.(json.Number)), string(right.(json.Number)))
	case "string":
		return strings.Compare(left.(string), right.(string)), true
	case "boolean":
		if op != "==" && op != "!=" {
			return 0, false
		}
		if left.(bool) == right.(bool) {
			return 0, true
		}
		return 1, true
	}
	return 0, false
}

func decodeJson(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// decimal is digits × 10^exp, with no leading or trailing zeros in digits,
// and empty digits for zero.
type decimal struct {
	negative bool
	digits   string
	exp      int64
}

// parseDecimal reads a JSON number without expanding its exponent, so a
// number such as 1e100000 costs no more than its text.
func parseDecimal(s string) (decimal, bool) {
	var d decimal
	if strings.HasPrefix(s, "-") {
		d.negative = true
		s = s[1:]
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil {
			return decimal{}, false
		}
		d.exp = exp
		s = s[:i]
	}
	whole, fraction, dotted := strings.Cut(s, ".")
	if whole == "" || !allDigits(whole) || !allDigits(fraction) || (dotted && fraction == "") {
		return decimal{}, false
	}
	digits := strings.TrimLeft(whole+fraction, "0")
	d.exp -= int64(len(fraction))
	trimmed := strings.TrimRight(digits, "0")
	d.exp += int64(len(digits) - len(trimmed))
	d.digits = trimmed
	if d.digits == "" {
		return decimal{}, true
	}
	return d, true
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// compareDecimals orders two JSON numbers exactly.
func compareDecimals(a, b string) (int, bool) {
	x, ok := parseDecimal(a)
	if !ok {
		return 0, false
	}
	y, ok := parseDecimal(b)
	if !ok {
		return 0, false
	}
	sign := func(d decimal) int {
		switch {
		case d.digits == "":
			return 0
		case d.negative:
			return -1
		}
		return 1
	}
	if sx, sy := sign(x), sign(y); sx != sy || sx == 0 {
		return compareInts(sx, sy), true
	}
	magnitude := compareMagnitudes(x, y)
	if x.negative {
		return -magnitude, true
	}
	return magnitude, true
}

// compareMagnitudes orders |x| and |y|, both non-zero: first by the place
// of the leading digit, then digit by digit.
func compareMagnitudes(x, y decimal) int {
	px, py := int64(len(x.digits))+x.exp, int64(len(y.digits))+y.exp
	if px != py {
		return compareInts(px, py)
	}
	n := min(len(x.digits), len(y.digits))
	if c := strings.Compare(x.digits[:n], y.digits[:n]); c != 0 {
		return c
	}
	// The longer has more digits after the shared ones, and the last of them
	// is not zero.
	return compareInts(len(x.digits), len(y.digits))
}

func compareInts[T int | int64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CheckAgainstSchemas looks for mistakes a condition can be seen to have at
// the grant: an action it requires that the mandate does not cover, a
// payload field that no covered action's schema declares, and a literal
// whose type no declaration of the field has. Schemas are those of the
// actions the mandate covers at the grant, with nil for one without a
// schema; then the payload cannot be checked at all, and neither can it
// when the mandate covers no action yet. Parts of a schema it does not read
// (references, combinations, additional properties) are taken to allow
// anything.
func (p Plan) CheckAgainstSchemas(patterns []string, schemas []json.RawMessage) error {
	checkPayload := len(schemas) > 0
	var decoded []any
	for _, raw := range schemas {
		v, ok := decodeJson(raw)
		if !ok {
			checkPayload = false
			break
		}
		decoded = append(decoded, v)
	}
	return p.walk(false, func(leaf Plan, negated bool) error {
		if leaf.Path[0] == varAction {
			// Only a key the condition requires must be covered: action !=
			// "x" and !(action in [...]) may name anything.
			requires := leaf.Op == "!=" && negated || (leaf.Op == "==" || leaf.Op == "in") && !negated
			if !requires {
				return nil
			}
			for _, raw := range append([]json.RawMessage{leaf.Value}, leaf.Values...) {
				var key string
				if len(raw) > 0 && json.Unmarshal(raw, &key) == nil && !Covers(patterns, key) {
					return conditionError("the condition requires the action %q, which the mandate does not cover", key)
				}
			}
			return nil
		}
		if !checkPayload {
			return nil
		}
		field := strings.Join(leaf.Path, ".")
		var literalKind string
		if leaf.Op != "has" {
			raw := leaf.Value
			if leaf.Op == "in" {
				raw = leaf.Values[0]
			}
			v, _ := decodeJson(raw)
			literalKind = jsonKind(v)
		}
		declared, typed := false, false
		for _, schema := range decoded {
			kinds, found := declaredTypes(schema, leaf.Path[1:])
			if !found {
				continue
			}
			declared = true
			if kinds == nil || literalKind == "" || slices.Contains(kinds, literalKind) {
				typed = true
			}
		}
		switch {
		case !declared:
			return conditionError("the condition names %s, which no action the mandate covers declares in its schema", field)
		case !typed:
			return conditionError("the condition compares %s with a %s, which it never is in the schemas of the actions the mandate covers", field, literalKind)
		}
		return nil
	})
}

// walk calls leaf for every comparison, in and has in the plan, with
// whether an odd number of nots is above it.
func (p Plan) walk(negated bool, leaf func(Plan, bool) error) error {
	switch p.Op {
	case "and", "or", "not":
		for _, arg := range p.Args {
			if err := arg.walk(negated != (p.Op == "not"), leaf); err != nil {
				return err
			}
		}
		return nil
	}
	return leaf(p, negated)
}

// declaredTypes follows a path through a JSON schema's properties. found
// is false when the schema says the field is not declared; kinds is nil
// when the field is declared without a type the check reads, or when the
// schema cannot be read that far.
func declaredTypes(schema any, path []string) (kinds []string, found bool) {
	node := schema
	for _, key := range path {
		object, ok := node.(map[string]any)
		if !ok {
			return nil, true
		}
		properties, hasProperties := object["properties"].(map[string]any)
		if !hasProperties {
			// A schema that lists no properties says nothing about names.
			return nil, true
		}
		if property, ok := properties[key]; ok {
			node = property
			continue
		}
		for _, open := range []string{"$ref", "allOf", "anyOf", "oneOf", "patternProperties", "if", "dependentSchemas"} {
			if _, ok := object[open]; ok {
				return nil, true
			}
		}
		// Extra fields a schema states it takes are declared too.
		for _, open := range []string{"additionalProperties", "unevaluatedProperties"} {
			if extra, ok := object[open]; ok && extra != false {
				return nil, true
			}
		}
		return nil, false
	}
	object, ok := node.(map[string]any)
	if !ok {
		return nil, true
	}
	switch declared := object["type"].(type) {
	case string:
		return schemaKinds([]any{declared}), true
	case []any:
		return schemaKinds(declared), true
	}
	return nil, true
}

// schemaKinds turns JSON schema types into jsonb_typeof names.
func schemaKinds(declared []any) []string {
	var kinds []string
	for _, t := range declared {
		switch t {
		case "integer", "number":
			kinds = append(kinds, "number")
		case "string", "boolean", "object", "array", "null":
			kinds = append(kinds, t.(string))
		}
	}
	return kinds
}

// Unmet names, for a payload the plan does not hold for, the comparisons
// that stand in the way and what the payload has instead, such as
// "payload.amount <= 500 (payload.amount is 750)". Empty when it holds.
func (p Plan) Unmet(action string, payload json.RawMessage) []string {
	doc, ok := decodeJson(payload)
	if !ok {
		return []string{"the payload is not JSON"}
	}
	return p.unmet(action, doc, false)
}

// unmet walks into what is not satisfied. Under a not, satisfied means
// false, and De Morgan makes the same walk right: an and that must be false
// fails through every part that is true, as an or that must be true fails
// through every part that is not.
func (p Plan) unmet(action string, payload any, negated bool) []string {
	value, known := p.eval(action, payload)
	if known && value != negated {
		return nil
	}
	switch p.Op {
	case "and", "or":
		var out []string
		for _, arg := range p.Args {
			out = append(out, arg.unmet(action, payload, negated)...)
		}
		return out
	case "not":
		if len(p.Args) == 1 {
			return p.Args[0].unmet(action, payload, !negated)
		}
	}
	return []string{p.describe(action, payload, negated)}
}

// describe writes a comparison back as text, with the value it found.
func (p Plan) describe(action string, payload any, negated bool) string {
	field := pathText(p.Path)
	var text string
	switch p.Op {
	case "has":
		text = "has(" + field + ")"
	case "in":
		values := make([]string, len(p.Values))
		for i, v := range p.Values {
			values[i] = string(v)
		}
		text = field + " in [" + strings.Join(values, ", ") + "]"
	case "and", "or", "not":
		text = "a part of the condition"
	default:
		text = field + " " + p.Op + " " + string(p.Value)
	}
	if negated {
		text = "!(" + text + ")"
	}
	if p.Op == "has" {
		if len(p.Path) < 2 {
			return text
		}
		parent, found := payload, true
		if len(p.Path) > 2 {
			parent, found = lookup(p.Path[:len(p.Path)-1], action, payload)
		}
		if _, isObject := parent.(map[string]any); !found || !isObject {
			return text + " (" + pathText(p.Path[:len(p.Path)-1]) + " is not an object)"
		}
		return text
	}
	value, found := lookup(p.Path, action, payload)
	if !found {
		return text + " (" + field + " is missing)"
	}
	raw, _ := json.Marshal(value)
	shown := string(raw)
	// Cut by characters, never inside one: the line must stay valid UTF-8.
	if runes := []rune(shown); len(runes) > 100 {
		shown = string(runes[:97]) + "..."
	}
	return text + " (" + field + " is " + shown + ")"
}

// pathText writes a path as a condition would: payload.a.b, or
// payload["a-b"] where a name is not an identifier.
func pathText(path []string) string {
	if len(path) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(path[0])
	for _, key := range path[1:] {
		if identifier.MatchString(key) {
			b.WriteString("." + key)
		} else {
			quoted, _ := json.Marshal(key)
			b.WriteString("[" + string(quoted) + "]")
		}
	}
	return b.String()
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
