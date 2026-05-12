/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// dispatcher_stress_test.go
//
// Purpose: compile a curated set of CEL expressions that collectively exercise
// the maximum number of distinct registered functions, so that the
// addNeededBindings probe logging (vendor/github.com/google/cel-go/cel/program.go)
// shows the widest possible spread of dispatcher entries across all three
// resolution paths:
//
//	PATH-1 DIRECT    disp.FindOverload(overloadID)    — per-overload binding key
//	PATH-2 FALLBACK  disp.FindOverload(fnName)        — singleton / dominant binding
//	PATH-3 BYPASSED  planner switch before dispatcher — _&&_ _||_ _==_ _!=_ _[_] _?._ _[?_] _?_:_
//
// Each test case targets one function family; the final case ("maximum-coverage")
// combines as many as possible into a single expression — that is the "golden" case
// for observing the maximum usedOIDs / neededFns ratio in the probe summary line:
//
//	[cel:probe:addNeededBindings] SUMMARY: totalFns=157  totalDeclaredOverloads=345
//	  usedOIDs=N  neededFns=M  boundEntries=K  savedFns=…  savedB≈…
//
// Run with:
//
//	go test -v -count=1 -run TestDispatcherStress \
//	  ./staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/ 2>&1 | \
//	  grep -E 'cel:probe:addNeededBindings SUMMARY|PASS|FAIL'
package cel

import (
	"strings"
	"testing"

	"k8s.io/apiserver/pkg/cel/environment"
)

// stressCase is one named group of expressions targeting a specific function
// family.  All expressions in a group are compiled separately so each produces
// its own dispatcher — this lets the probe show per-group binding statistics.
type stressCase struct {
	// name identifies the function family exercised.
	name string
	// note documents which dispatcher paths each expression is expected to use.
	// PATH-1 = direct overload-ID key, PATH-2 = fn-name fallback, PATH-3 = planner-bypassed.
	note string
	// expressions are compiled individually; each gets its own env.Program() call.
	expressions []string
	// hasAuthorizer enables the authorizer variable in the environment.
	hasAuthorizer bool
}

// TestDispatcherStress compiles expressions that maximise the number of distinct
// function bindings placed into the per-program dispatcher.  It is not a
// functional test — all expressions must compile without error, but runtime
// evaluation is not performed here.  The interesting output is the stderr probe
// lines emitted by addNeededBindings inside vendor/…/cel/program.go.
func TestDispatcherStress(t *testing.T) {
	cases := []stressCase{

		// ── Group 1: arithmetic and comparison operators ─────────────────────────
		// _<_  _<=_  _>_  _>=_  _+_  _-_  _*_  _/_  _%_  -_  !_
		// None of these are in the planner special-case switch, so ALL consult the
		// dispatcher via PATH-2 FALLBACK (singleton binary/unary bindings).
		// Overload IDs resolved by the type-checker:
		//   less_int64, less_equals_int64, greater_int64, greater_equals_int64,
		//   add_int64, subtract_int64, multiply_int64, divide_int64, modulo_int64,
		//   negate_int64, logical_not
		{
			name: "arithmetic-and-comparison-operators",
			note: "PATH-2 FALLBACK for all (singleton bindings — none are in the planner switch)",
			expressions: []string{
				// Comparison — 4 singleton fns (_<_ _<=_ _>_ _>=_)
				`1 < 2 && 1 <= 1 && 2 > 1 && 2 >= 2`,
				// Arithmetic — 5 singleton fns (_+_ _-_ _*_ _/_ _%_)
				`(3 + 2) - 1 == 4 && 3 * 2 == 6 && 8 / 4 == 2 && 7 % 3 == 1`,
				// Unary — 2 more singleton fns (-_ !_)
				`-1 < 0 && !(false)`,
				// Cross-type numeric comparisons (extends less/greater to mixed int/double)
				`1 < 2.0 && 1.5 <= 2 && 3 > 2.5 && 2.0 >= 2`,
				// String comparison (less_string, greater_string variants via singleton)
				`'apple' < 'banana' && 'zebra' > 'ant'`,
				// Combined: hit all 11 OIDs in one expression
				`1 < 2 && 1 <= 1 && 2 > 1 && 2 >= 2 &&
				 (3 + 2) - 1 == 4 && 3 * 2 == 6 && 8 / 4 == 2 && 7 % 3 == 1 &&
				 -1 < 0 && !(false)`,
			},
		},

		// ── Group 2: membership (@in / _in_) ────────────────────────────────────
		// @in is NOT in the planner switch → uses dispatcher.
		// Singleton binary binding keyed by "@in"; resolved overloads in_list / in_map
		// fall back to the "@in" fn-name key → PATH-2 FALLBACK.
		{
			name: "membership-in-operator",
			note: "PATH-2 FALLBACK via @in singleton (in_list, in_map both fall back to @in fn-name key)",
			expressions: []string{
				`'a' in ['a', 'b', 'c']`,
				`'key' in {'key': 'val', 'other': 'val2'}`,
				`3 in [1, 2, 3, 4]`,
			},
		},

		// ── Group 3: comprehension helpers (@not_strictly_false) ────────────────
		// @not_strictly_false has a per-overload UnaryBinding → Case C → 2 entries:
		//   "not_strictly_false" (PATH-1 DIRECT) + "@not_strictly_false" (PATH-2 FALLBACK).
		// Triggered by all() / exists() / exists_one() macro expansions.
		{
			name: "comprehension-macros-not-strictly-false",
			note: "PATH-1 DIRECT for not_strictly_false overload-ID key + PATH-2 FALLBACK for @not_strictly_false fn-name key",
			expressions: []string{
				`[1, 2, 3].all(x, x > 0)`,
				`[1, 2, 3].exists(x, x > 2)`,
				`[1, 2, 3].exists_one(x, x == 2)`,
				`[1, 2, 3].filter(x, x > 1) == [2, 3]`,
				`[1, 2, 3].map(x, x * 2) == [2, 4, 6]`,
			},
		},

		// ── Group 4: standard CEL string member functions ────────────────────────
		// startsWith / endsWith / contains: per-overload BinaryBinding → Case C →
		//   2 entries each (PATH-1 for overload-ID, PATH-2 for fn-name fallback).
		// matches: SingletonBinaryBinding → Case A → 1 entry keyed by fn name
		//   ("matches") → PATH-2 FALLBACK for resolved OID "matches_string".
		// size: SingletonUnaryBinding → Case A → 1 entry keyed by "size" →
		//   PATH-2 FALLBACK for all size_* variants.
		{
			name: "standard-string-member-functions",
			note: "startsWith/endsWith/contains: PATH-1+PATH-2 (Case C). matches/size: PATH-2 singleton (Case A).",
			expressions: []string{
				`object.metadata.name.startsWith('app-')`,
				`object.metadata.name.endsWith('-prod')`,
				`object.metadata.name.contains('service')`,
				`object.metadata.name.matches('^[a-z][a-z0-9-]*$')`,
				`object.metadata.name.size() <= 63`,
				`size(object.metadata.name) > 0`,
				`size(['a', 'b', 'c']) == 3`,
				`size({'k': 'v'}) == 1`,
				// Combined: all 4+size in one expression
				`object.metadata.name.startsWith('app-') && object.metadata.name.size() > 0 &&
				 object.metadata.name.endsWith('-svc') && object.metadata.name.contains('-') &&
				 object.metadata.name.matches('^app-.*-svc$')`,
			},
		},

		// ── Group 5: string extension functions (ext.Strings) ───────────────────
		// lowerAscii, upperAscii, trim, replace, split, join, indexOf, lastIndexOf,
		// substring, find, findAll, charAt, strings.quote
		// All are separate FunctionDecls; most have per-overload bindings (PATH-1).
		{
			name: "string-extension-functions",
			note: "PATH-1 DIRECT for most ext string functions (per-overload BinaryBinding/UnaryBinding)",
			expressions: []string{
				`object.metadata.name.lowerAscii() == object.metadata.name`,
				`object.metadata.name.upperAscii().size() > 0`,
				`object.metadata.name.trim() == object.metadata.name`,
				`object.metadata.name.replace('-', '_').contains('_') || true`,
				`object.metadata.name.split('-').size() >= 1`,
				`['app', 'svc', 'v1'].join('-').size() > 0`,
				`object.metadata.name.indexOf('-') >= -1`,
				`object.metadata.name.lastIndexOf('-') >= -1`,
				`object.metadata.name.charAt(0).size() <= 1`,
				`object.metadata.name.substring(0, object.metadata.name.size()).size() >= 0`,
				`object.metadata.name.find('[a-z]') != '' || true`,
				`object.metadata.name.findAll('[a-z]').size() >= 0`,
				`strings.quote(object.metadata.name).startsWith('"')`,
				// Combined: 13 distinct ext string functions in one expression
				`object.metadata.name.lowerAscii().size() > 0 &&
				 object.metadata.name.upperAscii().trim().size() > 0 &&
				 object.metadata.name.replace('-', '_').indexOf('_') >= -1 &&
				 object.metadata.name.split('-').join('-') == object.metadata.name &&
				 object.metadata.name.substring(0, 1).charAt(0).size() <= 1 &&
				 object.metadata.name.find('[a-z0-9]') != '' &&
				 object.metadata.name.findAll('[a-z]').size() >= 0 &&
				 strings.quote(object.metadata.name).lastIndexOf('"') > 0`,
			},
		},

		// ── Group 6: type conversion functions ──────────────────────────────────
		// int(), uint(), double(), bool(), string(), bytes(), type(), dyn()
		// Most have per-overload UnaryBindings (PATH-1 DIRECT for specific OIDs like
		// int64_to_string, string_to_int64) plus a fn-name funcDispatch entry (PATH-2).
		{
			name: "type-conversion-functions",
			note: "PATH-1 DIRECT for each specific conversion overload + PATH-2 fn-name funcDispatch entry",
			expressions: []string{
				// int() overloads
				`int('5') == 5`,
				`int(3.7) == 3`,
				// uint() overloads
				`uint(5) == uint(5u)`,
				// double() overloads
				`double(3) > 2.9`,
				`double('3.14') > 3.0`,
				// string() overloads — bool_to_string, int64_to_string, double_to_string
				`string(true) == 'true'`,
				`string(42) == '42'`,
				`string(3.14).size() > 0`,
				`string(bytes('abc')) == 'abc'`,
				// bool() overloads — string_to_bool
				`bool('true') == true`,
				// bytes() overloads — string_to_bytes
				`bytes('hello').size() == 5`,
				// type()
				`type(1) == type(0)`,
				`type('hello') == type('')`,
				`type([1,2,3]) == type([])`,
				// dyn()
				`dyn(42) == 42`,
				// duration() and string() round-trip
				`string(duration('1h30m')) == '5400s'`,
				// timestamp() and string() round-trip
				`string(timestamp('2024-01-15T00:00:00Z')).size() > 0`,
			},
		},

		// ── Group 7: timestamp member functions ─────────────────────────────────
		// getFullYear, getMonth, getDayOfMonth, getDayOfWeek, getDayOfYear, getDate,
		// getHours, getMinutes, getSeconds, getMilliseconds — each is a separate
		// FunctionDecl with two MemberOverloads (without-TZ and with-TZ variants).
		// All go through PATH-1 DIRECT for the no-TZ overload or
		// PATH-1 DIRECT for the with-TZ overload.
		{
			name: "timestamp-member-functions",
			note: "PATH-1 DIRECT for all (per-overload UnaryBinding and BinaryBinding per function)",
			expressions: []string{
				`timestamp('2024-06-15T10:30:45.123Z').getFullYear() == 2024`,
				`timestamp('2024-06-15T10:30:45.123Z').getMonth() == 5`,
				`timestamp('2024-06-15T10:30:45.123Z').getDayOfMonth() == 14`,
				`timestamp('2024-06-15T10:30:45.123Z').getDayOfWeek() == 6`,
				`timestamp('2024-06-15T10:30:45.123Z').getDayOfYear() >= 0`,
				`timestamp('2024-06-15T10:30:45.123Z').getDate() == 15`,
				`timestamp('2024-06-15T10:30:45.123Z').getHours() == 10`,
				`timestamp('2024-06-15T10:30:45.123Z').getMinutes() == 30`,
				`timestamp('2024-06-15T10:30:45.123Z').getSeconds() == 45`,
				`timestamp('2024-06-15T10:30:45.123Z').getMilliseconds() == 123`,
				// With-TZ overloads
				`timestamp('2024-06-15T10:30:45Z').getFullYear('UTC') == 2024`,
				`timestamp('2024-06-15T10:30:45Z').getHours('America/Los_Angeles') >= 0`,
				// Combined: all 10 no-TZ timestamp functions in one expression
				`timestamp('2024-06-15T10:30:45.500Z').getFullYear() == 2024 &&
				 timestamp('2024-06-15T10:30:45.500Z').getMonth() == 5 &&
				 timestamp('2024-06-15T10:30:45.500Z').getDate() == 15 &&
				 timestamp('2024-06-15T10:30:45.500Z').getDayOfMonth() == 14 &&
				 timestamp('2024-06-15T10:30:45.500Z').getDayOfWeek() == 6 &&
				 timestamp('2024-06-15T10:30:45.500Z').getDayOfYear() >= 0 &&
				 timestamp('2024-06-15T10:30:45.500Z').getHours() == 10 &&
				 timestamp('2024-06-15T10:30:45.500Z').getMinutes() == 30 &&
				 timestamp('2024-06-15T10:30:45.500Z').getSeconds() == 45 &&
				 timestamp('2024-06-15T10:30:45.500Z').getMilliseconds() == 500`,
			},
		},

		// ── Group 8: list extension functions ───────────────────────────────────
		// first, last, slice, reverse, flatten, distinct, sort, isSorted,
		// sum, min, max, lists.range, join (also a string ext)
		{
			name: "list-extension-functions",
			note: "PATH-1 DIRECT for Kubernetes list ext functions (per-overload bindings)",
			expressions: []string{
				// first() and last() return optional_type — must be unwrapped with .orValue()
				`['a', 'b', 'c'].first().orValue('') == 'a'`,
				`['a', 'b', 'c'].last().orValue('') == 'c'`,
				`['a', 'b', 'c'].slice(1, 2) == ['b']`,
				`['c', 'b', 'a'].reverse() == ['a', 'b', 'c']`,
				`[['a', 'b'], ['c']].flatten() == ['a', 'b', 'c']`,
				`['a', 'b', 'a', 'c'].distinct() == ['a', 'b', 'c']`,
				`['c', 'a', 'b'].sort() == ['a', 'b', 'c']`,
				`['a', 'b', 'c'].isSorted()`,
				`['c', 'b', 'a'].isSorted() == false`,
				`[1, 2, 3].sum() == 6`,
				`[1, 2, 3].min() == 1`,
				`[1, 2, 3].max() == 3`,
				`lists.range(5) == [0, 1, 2, 3, 4]`,
				`['a', 'b', 'c'].join('-') == 'a-b-c'`,
				// lists.range returns list(int), so join needs a .map(x, string(x)) step first
				`lists.range(3).map(x, string(x)).join(',') == '0,1,2'`,
				// Combined: all list ext functions in one expression
				`['c', 'a', 'b'].sort().first().orValue('') == 'a' &&
				 ['c', 'a', 'b'].sort().last().orValue('') == 'c' &&
				 ['c', 'a', 'b'].reverse().first().orValue('') == 'b' &&
				 [['x'], ['y', 'z']].flatten().size() == 3 &&
				 ['a', 'b', 'a'].distinct().size() == 2 &&
				 ['a', 'b', 'c'].slice(0, 2).size() == 2 &&
				 [3, 1, 2].sort().isSorted() &&
				 [10, 20, 30].sum() == 60 &&
				 [10, 20, 30].min() == 10 &&
				 [10, 20, 30].max() == 30 &&
				 lists.range(3).size() == 3 &&
				 lists.range(3).map(x, string(x)).join(',') == '0,1,2'`,
			},
		},

		// ── Group 9: sets extension functions ───────────────────────────────────
		// sets.contains, sets.equivalent, sets.intersects
		{
			name: "sets-extension-functions",
			note: "PATH-1 DIRECT (per-overload FunctionBinding on each sets.* function)",
			expressions: []string{
				`sets.contains(['a', 'b', 'c'], ['a', 'b'])`,
				`sets.contains(['a', 'b', 'c'], ['a', 'b', 'c', 'd']) == false`,
				`sets.equivalent(['a', 'b', 'c'], ['c', 'b', 'a'])`,
				`sets.equivalent(['a', 'b'], ['a', 'b', 'c']) == false`,
				`sets.intersects(['a', 'b', 'c'], ['b', 'c', 'd'])`,
				`sets.intersects(['a', 'b'], ['c', 'd']) == false`,
				// Combined: all 3 sets functions in one expression
				`sets.contains(['a', 'b', 'c'], ['a']) &&
				 sets.equivalent(['x', 'y'], ['y', 'x']) &&
				 sets.intersects(['1', '2'], ['2', '3'])`,
			},
		},

		// ── Group 10: optional type functions ───────────────────────────────────
		// optional.of, optional.none, optional.ofNonZeroValue, orValue, hasValue
		{
			name: "optional-type-functions",
			note: "PATH-1/PATH-2 depending on optional function binding style in cel-go ext",
			expressions: []string{
				`optional.of('value').hasValue()`,
				`optional.none().hasValue() == false`,
				`optional.of(42).orValue(0) == 42`,
				`optional.none().orValue(99) == 99`,
				`optional.ofNonZeroValue(0) == optional.none()`,
				`optional.ofNonZeroValue(1).hasValue()`,
				// Combined: all optional functions
				`optional.of(42).hasValue() &&
				 optional.none().orValue(0) == 0 &&
				 optional.ofNonZeroValue('').hasValue() == false &&
				 optional.ofNonZeroValue('x').orValue('def') == 'x'`,
			},
		},

		// ── Group 11: URL functions (Kubernetes URLs library) ───────────────────
		// url(), isURL(), getScheme(), getHost(), getHostname(), getPort(),
		// getEscapedPath(), getQuery()
		{
			name: "url-library-functions",
			note: "PATH-1 or PATH-2 depending on binding style in the Kubernetes URLs library",
			expressions: []string{
				`isURL('https://example.com/path?q=1')`,
				`isURL('not-a-url') == false`,
				`url('https://example.com/path?q=1').getScheme() == 'https'`,
				`url('https://example.com:8080/path').getHost() == 'example.com:8080'`,
				`url('https://example.com:8080/path').getHostname() == 'example.com'`,
				`url('https://example.com:8080/path').getPort() == '8080'`,
				`url('https://example.com/some/path').getEscapedPath() == '/some/path'`,
				// getQuery() returns map(string, list(string)), not a plain string — check its size
				`url('https://example.com/path?foo=bar&baz=qux').getQuery().size() == 2`,
				// Combined: all URL functions in one expression (getQuery returns a map)
				`isURL('https://api.example.com:443/v1/resources?limit=10') &&
				 url('https://api.example.com:443/v1/resources?limit=10').getScheme() == 'https' &&
				 url('https://api.example.com:443/v1/resources?limit=10').getHostname() == 'api.example.com' &&
				 url('https://api.example.com:443/v1/resources?limit=10').getPort() == '443' &&
				 url('https://api.example.com:443/v1/resources?limit=10').getEscapedPath() == '/v1/resources' &&
				 url('https://api.example.com:443/v1/resources?limit=10').getQuery().size() == 1`,
			},
		},

		// ── Group 12: IP library functions ──────────────────────────────────────
		// ip(), isIP(), family(), isLoopback(), isGlobalUnicast(), isLinkLocalUnicast(),
		// isLinkLocalMulticast(), isUnspecified()
		{
			name: "ip-library-functions",
			note: "PATH-1 or PATH-2 depending on binding style in the Kubernetes IP library",
			expressions: []string{
				`isIP('192.168.1.1')`,
				`isIP('not-an-ip') == false`,
				`isIP('::1')`,
				`ip('192.168.1.1').family() == 4`,
				`ip('::1').family() == 6`,
				`ip('127.0.0.1').isLoopback()`,
				`ip('192.168.1.1').isLoopback() == false`,
				`ip('192.168.1.1').isGlobalUnicast()`,
				`ip('169.254.1.1').isLinkLocalUnicast()`,
				`ip('ff02::1').isLinkLocalMulticast()`,
				`ip('0.0.0.0').isUnspecified()`,
				`ip('192.168.1.1').isUnspecified() == false`,
				// Combined: all IP member functions in one expression
				`isIP('10.0.0.1') &&
				 ip('10.0.0.1').family() == 4 &&
				 ip('10.0.0.1').isLoopback() == false &&
				 ip('10.0.0.1').isGlobalUnicast() &&
				 ip('10.0.0.1').isLinkLocalUnicast() == false &&
				 ip('10.0.0.1').isLinkLocalMulticast() == false &&
				 ip('10.0.0.1').isUnspecified() == false &&
				 isIP('::1') &&
				 ip('::1').family() == 6 &&
				 ip('::1').isLoopback()`,
			},
		},

		// ── Group 13: CIDR library functions ────────────────────────────────────
		// cidr(), isCIDR(), prefixLength(), masked(), containsIP(), containsCIDR()
		{
			name: "cidr-library-functions",
			note: "PATH-1 or PATH-2 depending on binding style in the Kubernetes CIDR library",
			expressions: []string{
				`isCIDR('192.168.0.0/16')`,
				`isCIDR('not-a-cidr') == false`,
				`cidr('192.168.0.0/16').prefixLength() == 16`,
				`cidr('10.0.0.0/8').prefixLength() == 8`,
				`cidr('192.168.1.1/24').masked() == cidr('192.168.1.0/24')`,
				`cidr('192.168.0.0/16').containsIP(ip('192.168.1.1'))`,
				`cidr('10.0.0.0/8').containsIP(ip('10.255.255.255'))`,
				`cidr('192.168.0.0/16').containsCIDR(cidr('192.168.1.0/24'))`,
				`cidr('192.168.1.0/24').containsCIDR(cidr('192.168.0.0/16')) == false`,
				// Combined: all CIDR functions
				`isCIDR('10.0.0.0/8') &&
				 cidr('10.0.0.0/8').prefixLength() == 8 &&
				 cidr('10.0.0.0/8').masked() == cidr('10.0.0.0/8') &&
				 cidr('10.0.0.0/8').containsIP(ip('10.1.2.3')) &&
				 cidr('10.0.0.0/8').containsCIDR(cidr('10.1.0.0/16'))`,
			},
		},

		// ── Group 14: Quantity library functions ─────────────────────────────────
		// quantity(), isQuantity(), asApproximateFloat(), asInteger(), isInteger(),
		// isGreaterThan(), isLessThan(), compareTo(), add(), sub(), sign()
		{
			name: "quantity-library-functions",
			note: "PATH-1 or PATH-2 depending on binding style in the Kubernetes Quantity library",
			expressions: []string{
				`isQuantity('1Gi')`,
				`isQuantity('not-a-quantity') == false`,
				`isQuantity('500m')`,
				`quantity('1000').asApproximateFloat() > 999.0`,
				`quantity('1').asInteger() == 1`,
				`quantity('1').isInteger()`,
				`quantity('1.5').isInteger() == false`,
				`quantity('2Gi').isGreaterThan(quantity('1Gi'))`,
				`quantity('512Mi').isLessThan(quantity('1Gi'))`,
				`quantity('1Gi').compareTo(quantity('1Gi')) == 0`,
				`quantity('2Gi').compareTo(quantity('1Gi')) > 0`,
				`quantity('1Gi').add(quantity('512Mi')).compareTo(quantity('1536Mi')) == 0`,
				`quantity('2Gi').sub(quantity('512Mi')).compareTo(quantity('1536Mi')) == 0`,
				`sign(quantity('-5')) == -1`,
				`sign(quantity('5')) == 1`,
				`sign(quantity('0')) == 0`,
				// Combined: all quantity functions in one expression
				`isQuantity('1Gi') &&
				 quantity('1Gi').asApproximateFloat() > 1e9 &&
				 quantity('1').asInteger() == 1 &&
				 quantity('1').isInteger() &&
				 quantity('2Gi').isGreaterThan(quantity('1Gi')) &&
				 quantity('512Mi').isLessThan(quantity('1Gi')) &&
				 quantity('1Gi').compareTo(quantity('1Gi')) == 0 &&
				 quantity('1Gi').add(quantity('1Gi')).compareTo(quantity('2Gi')) == 0 &&
				 quantity('2Gi').sub(quantity('1Gi')).compareTo(quantity('1Gi')) == 0 &&
				 sign(quantity('-1')) == -1`,
			},
		},

		// ── Group 15: Semver library functions ──────────────────────────────────
		// semver(), isSemver(), major(), minor(), patch(),
		// isGreaterThan(), isLessThan(), compareTo()
		{
			name: "semver-library-functions",
			note: "PATH-1 or PATH-2 depending on binding style in the Kubernetes Semver library",
			expressions: []string{
				`isSemver('1.2.3')`,
				`isSemver('not-semver') == false`,
				`isSemver('1.0.0-alpha.1')`,
				`semver('2.5.1').major() == 2`,
				`semver('2.5.1').minor() == 5`,
				`semver('2.5.1').patch() == 1`,
				`semver('2.0.0').isGreaterThan(semver('1.9.9'))`,
				`semver('1.0.0').isLessThan(semver('2.0.0'))`,
				`semver('1.2.3').compareTo(semver('1.2.3')) == 0`,
				`semver('2.0.0').compareTo(semver('1.0.0')) > 0`,
				// Combined: all semver functions in one expression
				`isSemver('1.2.3') &&
				 semver('1.2.3').major() == 1 &&
				 semver('1.2.3').minor() == 2 &&
				 semver('1.2.3').patch() == 3 &&
				 semver('2.0.0').isGreaterThan(semver('1.0.0')) &&
				 semver('1.0.0').isLessThan(semver('2.0.0')) &&
				 semver('1.0.0').compareTo(semver('1.0.0')) == 0`,
			},
		},

		// ── Group 16: Format validation functions ────────────────────────────────
		// format.dns1035Label(), format.dns1123Label(), format.dns1123Subdomain(),
		// format.labelValue(), format.qualifiedName(), format.uri(), format.uuid()
		// Each factory returns a Format object; .validate(s) returns optional.none if
		// valid, or optional(list(string)) of error messages if invalid.
		// So: !validate.hasValue() ↔ valid,  validate.hasValue() ↔ invalid.
		{
			name: "format-library-functions",
			note: "PATH-1 or PATH-2 depending on binding style in the Kubernetes Format library",
			expressions: []string{
				// validate() returns optional_type(list(string)):
				//   optional.none  → valid input (no errors)
				//   optional(list) → invalid input (list of error messages)
				`!format.dns1035Label().validate('my-app').hasValue()`,
				`format.dns1035Label().validate('My-App').hasValue()`,
				`!format.dns1123Label().validate('my-app-123').hasValue()`,
				`!format.dns1123Subdomain().validate('my.app.example.com').hasValue()`,
				`!format.labelValue().validate('my-value').hasValue()`,
				`!format.qualifiedName().validate('myapp.example.com/resource').hasValue()`,
				`!format.uri().validate('https://example.com/path').hasValue()`,
				`!format.uuid().validate('550e8400-e29b-41d4-a716-446655440000').hasValue()`,
				// Combined: multiple format validators + value() to get error messages
				`!format.dns1035Label().validate(object.metadata.name).hasValue() ||
				 format.dns1035Label().validate(object.metadata.name).orValue([]).size() > 0`,
			},
		},

		// ── Group 17: authorizer functions ──────────────────────────────────────
		// group(), resource(), subresource(), namespace(), name(), check(), allowed(),
		// reason(), serviceAccount() — all Kubernetes-specific authz functions.
		// Note: namespace() is a member on ResourceCheck (not on Authorizer directly).
		// The chain is: authorizer → .group() → .resource() → .namespace() → .check() → .allowed()
		{
			name:          "authorizer-library-functions",
			note:          "PATH-1 or PATH-2 depending on binding style in the Kubernetes Authz library",
			hasAuthorizer: true,
			expressions: []string{
				`authorizer.group('') != null`,
				`authorizer.group('apps').resource('deployments').check('create').allowed()`,
				// reason() returns a string — compare size not null
				`authorizer.group('apps').resource('deployments').check('update').reason().size() >= 0`,
				// namespace() is a method on ResourceCheck, not on Authorizer
				`authorizer.group('').resource('configmaps').subresource('status').check('get').allowed() || true`,
				`authorizer.group('').resource('pods').namespace('default').check('list').allowed() || true`,
			},
		},

		// ── Group 18: MAXIMUM-COVERAGE single expression ─────────────────────────
		//
		// One expression that drives the most possible distinct function families
		// into a single env.Program() dispatcher.  This is the "golden expression"
		// for measuring patch effectiveness.
		//
		// Expected probe output (approximate, will vary with library versions):
		//   [cel:probe:addNeededBindings] SUMMARY:
		//     totalFns=157  totalDeclaredOverloads=345
		//     usedOIDs=~70  neededFns=~55  boundEntries=~80
		//     savedFns=~102  savedB≈~82000
		//
		// Compare to the unpatched baseline where ALL 352 entries were always bound
		// regardless of which functions the expression actually uses.
		{
			name: "maximum-coverage-single-expression",
			note: "Exercises ~17 function families in one expression — maximises usedOIDs/neededFns in probe SUMMARY",
			expressions: []string{
				// No CEL comments inside backtick strings — CEL has no comment syntax.
				// Groups (for readability in the Go source) are just whitespace-separated.
				// std string (startsWith/endsWith/contains/matches/size)
				`object.metadata.name.startsWith('app') &&
				 object.metadata.name.endsWith('-svc') &&
				 object.metadata.name.contains('-') &&
				 object.metadata.name.matches('^[a-z]') &&
				 object.metadata.name.size() <= 253 &&
				 object.metadata.name.lowerAscii().size() > 0 &&
				 object.metadata.name.trim() == object.metadata.name &&
				 object.metadata.name.replace('-', '_').indexOf('_') >= 0 &&
				 object.metadata.name.split('-').join('-') == object.metadata.name &&
				 object.metadata.name.findAll('[a-z]').size() > 0 &&
				 object.metadata.name.find('[a-z]') != '' &&
				 object.metadata.name.substring(0, 3).size() <= 3 &&
				 strings.quote(object.metadata.name).lastIndexOf('"') > 0 &&
				 ['app', 'svc', 'web'].all(s, s.size() > 0) &&
				 ['app', 'svc', 'web'].exists(s, s.startsWith('a')) &&
				 ['c', 'a', 'b'].sort().first().orValue('') == 'a' &&
				 ['c', 'a', 'b'].sort().last().orValue('') == 'c' &&
				 ['c', 'a', 'b'].reverse().first().orValue('') == 'b' &&
				 [['x'], ['y', 'z']].flatten().size() == 3 &&
				 ['a', 'b', 'a'].distinct().size() == 2 &&
				 [3, 1, 2].sort().isSorted() &&
				 [10, 20, 30].sum() == 60 &&
				 [10, 20, 30].min() == 10 &&
				 [10, 20, 30].max() == 30 &&
				 ['a', 'b', 'c'].slice(0, 2).size() == 2 &&
				 lists.range(3).size() == 3 &&
				 sets.contains(['a', 'b', 'c'], ['a']) &&
				 sets.equivalent(['x', 'y'], ['y', 'x']) &&
				 sets.intersects(['1', '2'], ['2', '3']) &&
				 optional.of('val').hasValue() &&
				 optional.none().orValue('default') == 'default' &&
				 optional.ofNonZeroValue('').hasValue() == false &&
				 'a' in ['a', 'b', 'c'] &&
				 !(false) &&
				 1 < 2 && 2 <= 2 && 3 > 2 && 3 >= 3 &&
				 (3 + 2) == 5 && 5 - 1 == 4 &&
				 string(42) == '42' &&
				 int('5') == 5 &&
				 double(3) > 2.9 &&
				 timestamp('2024-06-15T10:30:45Z').getFullYear() == 2024 &&
				 timestamp('2024-06-15T10:30:45Z').getMonth() == 5 &&
				 timestamp('2024-06-15T10:30:45Z').getHours() == 10 &&
				 isURL('https://example.com') &&
				 url('https://api.example.com:8080/v1').getScheme() == 'https' &&
				 url('https://api.example.com:8080/v1').getHostname() == 'api.example.com' &&
				 url('https://api.example.com:8080/v1').getPort() == '8080' &&
				 isIP('10.0.0.1') &&
				 ip('10.0.0.1').family() == 4 &&
				 ip('127.0.0.1').isLoopback() &&
				 ip('10.0.0.1').isGlobalUnicast() &&
				 isCIDR('10.0.0.0/8') &&
				 cidr('10.0.0.0/8').prefixLength() == 8 &&
				 cidr('10.0.0.0/8').containsIP(ip('10.1.2.3')) &&
				 isQuantity('1Gi') &&
				 quantity('1Gi').asInteger() >= 0 &&
				 quantity('2Gi').isGreaterThan(quantity('1Gi')) &&
				 quantity('1Gi').compareTo(quantity('1Gi')) == 0 &&
				 quantity('1Gi').add(quantity('1Gi')).compareTo(quantity('2Gi')) == 0 &&
				 sign(quantity('-1')) == -1 &&
				 isSemver('1.2.3') &&
				 semver('1.2.3').major() == 1 &&
				 semver('1.2.3').minor() == 2 &&
				 semver('1.2.3').patch() == 3 &&
				 semver('2.0.0').isGreaterThan(semver('1.0.0')) &&
				 semver('1.0.0').compareTo(semver('1.0.0')) == 0`,
			},
		},
	}

	compiler := NewCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("dispatcher paths: %s", tc.note)

			opts := OptionalVariableDeclarations{
				HasParams:     false,
				HasAuthorizer: tc.hasAuthorizer,
			}

			for i, expr := range tc.expressions {
				result := compiler.CompileCELExpression(
					&fakeValidationCondition{Expression: expr},
					opts,
					environment.NewExpressions,
				)
				if result.Error != nil {
					display := strings.ReplaceAll(expr, "\n", " ")
					if len(display) > 120 {
						display = display[:120] + "…"
					}
					t.Errorf("expr[%d] compile error: %v\n  expr: %s", i, result.Error, display)
				}
			}
			t.Logf("all %d expressions compiled — see [cel:probe:addNeededBindings] in stderr", len(tc.expressions))
		})
	}
}
