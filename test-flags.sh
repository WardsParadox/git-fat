#!/bin/bash
# Exercises every subcommand and flag against a local-path remote. No network.
# Requires git-fat on PATH.

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
FAILS=0
export GIT_CONFIG_GLOBAL="$T/gitconfig" GIT_CONFIG_NOSYSTEM=1
git config --global user.name tester
git config --global user.email tester@example.invalid
git config --global init.defaultBranch main
unset GIT_FAT_VERBOSE GIT_FAT_PROFILE GIT_FAT_REGION GIT_FAT_JOBS GIT_FAT_ACCELERATE AWS_PROFILE

pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; [ -n "$2" ] && echo "       $2"; FAILS=$((FAILS + 1)); }

# run <description> <expected-rc> <command...>; sets OUT and RC.
run() {
    local desc=$1 want=$2
    shift 2
    OUT=$("$@" 2>&1)
    RC=$?
    if [ "$RC" -ne "$want" ]; then
        fail "$desc (exit $RC, want $want)" "$OUT"
        return 1
    fi
    pass "$desc"
}
has() { # has <description> <fixed-string>
    if printf '%s\n' "$OUT" | grep -qF -- "$2"; then pass "$1"; else fail "$1: missing '$2'" "$OUT"; fi
}
lacks() {
    if printf '%s\n' "$OUT" | grep -qF -- "$2"; then fail "$1: unexpected '$2'" "$OUT"; else pass "$1"; fi
}
exists() { if [ -e "$2" ]; then pass "$1"; else fail "$1: $2 missing"; fi; }
absent() { if [ ! -e "$2" ]; then pass "$1"; else fail "$1: $2 present"; fi; }
objdigest() { printf '%s\n' "$1" | git fat filter-clean | awk '{print $3}'; }

STORE="$T/store"
mkdir "$T/work" && cd "$T/work" || exit 1
git init -q repo && cd repo || exit 1
git fat init >/dev/null
echo '*.fat filter=fat -text' > .gitattributes
printf '[rsync]\nremote = %s\n' "$STORE" > .gitfat
git add .gitattributes .gitfat && git commit -qm init
echo 'content A' > a.fat; git add a.fat; git commit -qm a
C2=$(git rev-parse HEAD)
echo 'content B' > b.fat; git add b.fat; git commit -qm b
MAIN=$(git branch --show-current)
git checkout -q -b other
echo 'content C' > c.fat; git add c.fat; git commit -qm c
git checkout -q "$MAIN"
OBJ=.git/fat/objects
A=$(git show "$MAIN":a.fat | awk '{print $3}')
B=$(git show "$MAIN":b.fat | awk '{print $3}')
C=$(git show other:c.fat | awk '{print $3}')
G=$(objdigest 'garbage nobody references')
cp -R "$OBJ" "$T/objects-backup"

echo "== status"
run "status with garbage" 0 git fat status
has "status lists garbage header" "Garbage objects:"
has "status lists the garbage object" "$G"
mv "$OBJ/$A" "$T/a.saved"
run "status with missing object" 0 git fat status
has "status lists orphan header" "Orphan objects:"
has "status lists the orphan object" "$A"
mv "$T/a.saved" "$OBJ/$A"
run "status --all" 0 git fat status --all
has "status --all lists objects from other branches" "$C"
run "status with a stray argument" 2 git fat status extra

echo "== push"
run "push" 0 git fat push
exists "push uploads HEAD objects" "$STORE/$A"
exists "push uploads HEAD objects (b)" "$STORE/$B"
absent "push leaves other-branch objects" "$STORE/$C"
run "push --all" 0 git fat push --all
exists "push --all uploads other-branch objects" "$STORE/$C"
absent "no temp files in the remote" "$STORE/$A.tmp"

fresh_clone() { # fresh_clone <name>
    git clone -q "$T/work/repo" "$T/work/$1" && cd "$T/work/$1" && git fat init >/dev/null
}

echo "== pull"
fresh_clone p1
run "pull" 0 git fat pull
has "pull restores a.fat" "Restoring $A"
[ "$(cat a.fat)" = "content A" ] && pass "a.fat content restored" || fail "a.fat content restored"
run "pull --all" 0 git fat pull --all
exists "pull --all fetches every ref" ".git/fat/objects/$C"

fresh_clone p2
run "pull <rev>" 0 git fat pull "$C2"
exists "pull <rev> fetches that commit's object" ".git/fat/objects/$A"
absent "pull <rev> skips later commits" ".git/fat/objects/$B"

fresh_clone p3
run "pull <rev> --all (flag after the revision)" 0 git fat pull "$C2" --all
exists "interspersed --all took effect" ".git/fat/objects/$C"

fresh_clone p4
run "pull -j 2 -- pattern" 0 git fat pull -j 2 -- 'a.fat'
exists "pattern pull fetches the match" ".git/fat/objects/$A"
absent "pattern pull skips the rest" ".git/fat/objects/$B"

fresh_clone p5
run "pull <rev> -j 4" 0 git fat pull "$C2" -j 4
exists "pull <rev> -j 4 fetches" ".git/fat/objects/$A"
run "pull with two revisions" 2 git fat pull "$C2" "$C2"
run "pull in an uninitialised clone" 1 bash -c "git clone -q '$T/work/repo' '$T/work/p6' && cd '$T/work/p6' && git fat pull"

echo "== transfer flags on a non-S3 remote"
cd "$T/work/repo"
echo 'content D' > d.fat; git add d.fat; git commit -qm d
for flags in "-j 1" "--jobs 3" "--part-size 6" "--region x" "--profile x" "--accelerate" "--no-accelerate" "-j 2 --region x --profile y --part-size 7 --accelerate"; do
    run "push $flags" 0 env GIT_FAT_VERBOSE=1 git fat push $flags
    case "$flags" in
    -j\ 1 | --jobs\ 3) lacks "push $flags needs no ignore note" "ignoring S3-only" ;;
    *) has "push $flags notes ignored options" "ignoring S3-only options" ;;
    esac
done
fresh_clone f1
for flags in "-j 1" "--jobs 3" "--part-size 6" "--region x" "--profile x" "--accelerate" "--no-accelerate"; do
    run "pull $flags" 0 env GIT_FAT_VERBOSE=1 git fat pull $flags
done
has "pull notes ignored options" "ignoring S3-only options"

echo "== gc"
cd "$T/work/repo"
run "gc --dry-run" 0 git fat gc --dry-run
has "dry run lists the garbage object" "$G"
lacks "dry run keeps other-branch objects" "$C"
exists "dry run removes nothing" "$OBJ/$G"
run "gc -n" 0 git fat gc -n
exists "gc -n removes nothing" "$OBJ/$G"
run "gc" 0 git fat gc
absent "gc removes unreferenced objects" "$OBJ/$G"
exists "gc keeps objects referenced by another branch" "$OBJ/$C"
exists "gc keeps HEAD objects" "$OBJ/$A"
run "gc --head-only --dry-run" 0 git fat gc --head-only --dry-run
has "head-only dry run lists other-branch object" "$C"
exists "head-only dry run removes nothing" "$OBJ/$C"
run "gc --head-only" 0 git fat gc --head-only
absent "gc --head-only removes other-branch objects" "$OBJ/$C"
exists "gc --head-only keeps HEAD objects" "$OBJ/$A"
rm -rf "$OBJ" && cp -R "$T/objects-backup" "$OBJ"

echo "== verify"
run "verify on a good store" 0 git fat verify
mv "$OBJ/$B" "$T/b.saved"
echo "not the right data" > "$OBJ/$B"
run "verify on a corrupt store" 1 git fat verify
has "verify reports the corruption" "Corrupted objects: 1"
rm -f "$OBJ/$B"; mv "$T/b.saved" "$OBJ/$B"

echo "== checkout"
fresh_clone k1
cp -R "$T/work/repo/$OBJ/." .git/fat/objects/
run "checkout" 0 git fat checkout
has "checkout restores from the store" "Restoring $A"
[ "$(cat a.fat)" = "content A" ] && pass "checkout wrote the file" || fail "checkout wrote the file"
fresh_clone k2
run "checkout with no objects" 0 git fat checkout
has "checkout reports unavailable data" "Data unavailable: $A"
run "checkout with a stray argument" 2 git fat checkout extra

echo "== find"
cd "$T/work/repo"
run "find" 0 git fat find 10
has "find lists large paths" "a.fat"
has "find prints attribute lines" "filter=fat -text"
run "find without a size" 2 git fat find
run "find with a bad size" 2 git fat find abc

echo "== profile"
run "profile" 0 git fat profile
has "profile prints the profile" "profile:"
has "profile prints jobs" "jobs:"
run "profile set" 0 git fat profile set foo
[ "$(git config fat.profile)" = "foo" ] && pass "profile set wrote fat.profile" || fail "profile set wrote fat.profile"
run "profile after set" 0 git fat profile
has "profile shows the source" "git config fat.profile"
run "profile --profile flag" 0 git fat profile --profile bar
has "profile flag wins" "bar (--profile flag)"
run "profile set without a name" 2 git fat profile set

echo "== s3-info"
run "s3-info on a non-S3 repo" 1 git fat s3-info
has "s3-info explains itself" "does not use S3"

echo "== version and usage"
run "version" 0 git fat version
has "version prints the name" "git-fat"
run "unknown subcommand" 2 git fat frobnicate
has "unknown subcommand prints usage" "Usage:"
run "unknown flag" 2 git fat status --nope
has "unknown flag prints usage" "not defined"
run "no subcommand" 2 git fat

echo "== --help"
for cmd in init status push pull gc verify checkout find index-filter profile s3-info version; do
    OUT=$(git fat "$cmd" --help 2>&1); RC=$?
    if { [ $RC -eq 0 ] || [ $RC -eq 2 ]; } && ! printf '%s' "$OUT" | grep -qE 'panic|goroutine'; then
        pass "$cmd --help"
    else
        fail "$cmd --help (exit $RC)" "$OUT"
    fi
done

echo "== empty repository"
cd "$T/work" && git init -q empty && cd empty && git fat init >/dev/null
printf '[rsync]\nremote = %s\n' "$STORE" > .gitfat
for cmd in status push pull; do
    run "$cmd with no commits" 0 git fat "$cmd"
    has "$cmd says there are no commits" "no commits yet"
    lacks "$cmd hides the raw git error" "ambiguous argument"
done
run "gc with no commits" 0 git fat gc
lacks "gc hides the raw git error" "ambiguous argument"

echo
if [ "$FAILS" -eq 0 ]; then echo "all flag tests passed"; else echo "$FAILS failed"; exit 1; fi
