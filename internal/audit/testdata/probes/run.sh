#!/bin/sh
# run.sh drives nanogo over every probe in this directory and reports what the
# compiler did with each one. It is the audit that keeps the documentation
# honest: a capability claim in README.md, doc.go or driver/help.go must name a
# probe here that shows the behaviour.
#
# Each probe is one directory holding one main package that exercises one Go
# construct. Every probe is run twice, once compiled by nanogo and once by the
# Go toolchain, and the two results are compared. gc is the oracle, so a probe
# needs no expected value written down.
#
# Each line of the report is one of:
#
#	REFUSED   nanogo refused the program, and the message follows
#	OK        nanogo compiled it and the program agreed with gc
#	WRONG     nanogo compiled it and the program disagreed with gc
#
# usage: NG=/path/to/nanogo sh run.sh [probe...]
set -u
NG=${NG:-nanogo}
cd "$(dirname "$0")"

# noise strips the three lines every nanogo build prints, which report the
# nanogo/gc split and are not part of a refusal.
noise='^nanogo: [0-9]* of [0-9]* packages\|^nanogo: the standard library\|^nanogo: the executable was'

run() { # run <compiler-command> <probe>; prints "exit|output"
	out=$(mktemp -d)/bin
	if ! err=$($1 build -o "$out" "./$2" 2>&1); then
		printf 'build-failed|%s' "$(printf '%s' "$err" | grep -v "$noise" | tr '\n' ' ')"
		return
	fi
	text=$("$out" 2>&1)
	printf '%s|%s' "$?" "$(printf '%s' "$text" | tr '\n' '/')"
}

for p in ${*:-$(ls -d */ | tr -d /)}; do
	got=$(run "$NG" "$p")
	case $got in
	build-failed\|*)
		printf '%-24s REFUSED %s\n' "$p" "${got#build-failed|}"
		continue
		;;
	esac
	want=$(run go "$p")
	if [ "$got" = "$want" ]; then
		printf '%-24s OK      exit=%s out=%s\n' "$p" "${got%%|*}" "${got#*|}"
	else
		printf '%-24s WRONG   nanogo=%s gc=%s\n' "$p" "$got" "$want"
	fi
done
