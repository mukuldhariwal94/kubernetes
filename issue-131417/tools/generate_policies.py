#!/usr/bin/env python3
"""Generate ValidatingAdmissionPolicy + Binding YAML with globally-unique CEL expressions.

Every match-condition and validation expression in the output is distinct
across the entire generated set, so the apiserver's process-wide CEL
program cache (issue #131417 / Patch 0001) cannot deduplicate any of them.
This is the worst case for the cache and lets you measure the apiserver's
true per-expression memory cost.

The generated bindings target a synthetic namespace selector
(memtest-target: "none") so the policies are registered and compiled but
do not fire on real workloads.

Usage:
  ./generate_policies.py --count 1000 --matches 5 --validations 10 -o policies.yaml
  kubectl apply -f policies.yaml

  # Cleanup:
  kubectl delete vap,vapb -l memtest=yes
  # or by name pattern:
  kubectl get vap -o name | grep memtest-policy- | xargs kubectl delete
"""

import argparse
import sys


def gen_match(p: int, i: int) -> str:
    # Unique on (p, i): the literal compared against is unique per expression,
    # so two expressions never share text. Returns true for any normal object.
    expr = f'object.metadata.name != "deny-match-{p}-{i}"'
    return f"  - name: m{p}-{i}\n    expression: '{expr}'"


def gen_validation(p: int, i: int) -> str:
    expr = f'object.metadata.name != "deny-val-{p}-{i}"'
    msg = f"validation {p}-{i} failed"
    return f"  - expression: '{expr}'\n    message: '{msg}'"


def gen_policy(p: int, n_match: int, n_val: int) -> str:
    lines = [
        "---",
        "apiVersion: admissionregistration.k8s.io/v1",
        "kind: ValidatingAdmissionPolicy",
        "metadata:",
        f"  name: memtest-policy-{p}",
        "  labels:",
        '    memtest: "yes"',
        "spec:",
        "  failurePolicy: Fail",
        "  matchConstraints:",
        "    resourceRules:",
        '    - apiGroups:   [""]',
        '      apiVersions: ["v1"]',
        '      operations:  ["CREATE", "UPDATE"]',
        '      resources:   ["configmaps"]',
    ]
    if n_match > 0:
        lines.append("  matchConditions:")
        for i in range(1, n_match + 1):
            lines.append(gen_match(p, i))
    lines.append("  validations:")
    for i in range(1, n_val + 1):
        lines.append(gen_validation(p, i))
    return "\n".join(lines)


def gen_binding(p: int) -> str:
    return "\n".join([
        "---",
        "apiVersion: admissionregistration.k8s.io/v1",
        "kind: ValidatingAdmissionPolicyBinding",
        "metadata:",
        f"  name: memtest-binding-{p}",
        "  labels:",
        '    memtest: "yes"',
        "spec:",
        f"  policyName: memtest-policy-{p}",
        "  validationActions: [Deny]",
        "  matchResources:",
        "    namespaceSelector:",
        "      matchLabels:",
        '        memtest-target: "none"',
    ])


def main() -> int:
    ap = argparse.ArgumentParser(
        description=(
            "Generate N ValidatingAdmissionPolicy + Binding pairs with "
            "globally-unique CEL expressions for cache memory testing."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Total unique expressions produced = count * (matches + validations).\n"
            "At ~60 KB per compiled cel.Program, expect roughly that much heap\n"
            "growth on the apiserver after all policies are registered."
        ),
    )
    ap.add_argument("--count", "-n", type=int, required=True,
                    help="number of policies (and bindings, 1:1) to generate")
    ap.add_argument("--matches", "-m", type=int, default=0,
                    help="match-condition expressions per policy (default: 0)")
    ap.add_argument("--validations", "-v", type=int, default=1,
                    help="validation expressions per policy (default: 1)")
    ap.add_argument("--output", "-o", default="-",
                    help="output file path; '-' for stdout (default: -)")
    ap.add_argument("--start-index", type=int, default=1,
                    help="starting policy index, useful for batching (default: 1)")
    args = ap.parse_args()

    if args.count < 1:
        print("error: --count must be >= 1", file=sys.stderr)
        return 2
    if args.matches < 0 or args.validations < 0:
        print("error: --matches and --validations must be >= 0", file=sys.stderr)
        return 2
    if args.validations == 0:
        print("warning: --validations 0 produces an invalid VAP "
              "(at least one validation is required)", file=sys.stderr)

    out = sys.stdout if args.output == "-" else open(args.output, "w")
    try:
        for offset in range(args.count):
            p = args.start_index + offset
            out.write(gen_policy(p, args.matches, args.validations))
            out.write("\n")
            out.write(gen_binding(p))
            out.write("\n")
    finally:
        if out is not sys.stdout:
            out.close()
            total_expr = args.count * (args.matches + args.validations)
            print(
                f"wrote {args.count} policies + {args.count} bindings "
                f"({total_expr} unique CEL expressions) to {args.output}",
                file=sys.stderr,
            )
    return 0


if __name__ == "__main__":
    sys.exit(main())
