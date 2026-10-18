#!/bin/bash -eu
#
# Publishes the current commit's files to the public repository as one new
# commit, without this repository's history.
#
# usage: scripts/publish.sh [--release | --branch NAME | --dry-run] "commit message"
#
# --release also points the public repository's release branch at the new
# commit, which makes its CI build the image and publish release
# v<VERSION>. Bump VERSION first.
#
# --branch NAME publishes to that branch instead of main, e.g. "rc" for a
# release candidate whose image is built by running Build image by hand on
# that branch, while main and the latest release stay as they are.
#
# --dry-run builds the public tree and runs the checks, but pushes nothing.
#
# Paths listed in .publish-exclude (one per line; a directory ends in /)
# stay out of the public tree, and so does that file itself. The script
# refuses to publish if any of them would still be in the tree.
#
# Setup, once:
#   git remote add public https://github.com/<owner>/glovebox.git
#   git config glovebox.publicName <name>
#   git config glovebox.publicEmail <id>+<user>@users.noreply.github.com
#   and list anything that must never be published (VIN, home addresses,
#   names, ...), one per line, in .git/info/publish-blocklist. That file is
#   inside .git, so it's never committed itself.

release=
branch=main
dry=
while [ $# -gt 0 ]
do
  case "$1" in
    --release) release=1 ;;
    --branch) branch=${2:?--branch needs a name}; shift ;;
    --dry-run) dry=1 ;;
    *) break ;;
  esac
  shift
done
if [ -n "$release" ] && [ "$branch" != main ]
then
  echo "--release publishes from main; leave out --branch"
  exit 1
fi
msg=${1:-}
if [ -z "$msg" ]
then
  sed -n '6p' "$0" | sed 's|^# ||'
  exit 1
fi
remote=${PUBLIC_REMOTE:-public}
# The public commit's author: its own name and address, never the ones in
# git config user.*, which this repository's own commits use.
name=$(git config glovebox.publicName || true)
email=$(git config glovebox.publicEmail || true)
if [ -z "$name" ] || [ -z "$email" ]
then
  echo "set the public commit author first:"
  echo "  git config glovebox.publicName <name>"
  echo "  git config glovebox.publicEmail <id>+<user>@users.noreply.github.com"
  exit 1
fi
if ! git diff --quiet HEAD || [ -n "$(git ls-files --others --exclude-standard)" ]
then
  echo "commit (or stash) your changes first: only committed files are published"
  exit 1
fi

blocklist=$(git rev-parse --git-path info/publish-blocklist)
if [ ! -s "$blocklist" ]
then
  echo "$blocklist is missing or empty; add what must never be published, one per line"
  exit 1
fi
# ignore blank lines and comments in the blocklist
patterns=$(mktemp)
trap 'rm -f "$patterns"' EXIT
grep -v -e '^[[:space:]]*$' -e '^#' "$blocklist" > "$patterns" || true
# The public tree: HEAD without the excluded paths, built in a temporary
# index so the working tree and the real index aren't touched.
exclude_file=.publish-exclude
if ! git cat-file -e "HEAD:$exclude_file" 2> /dev/null
then
  echo "HEAD has no $exclude_file; refusing to publish (it lists what stays private)"
  exit 1
fi
excludes=$(mktemp)
index=$(mktemp)
trap 'rm -f "$patterns" "$excludes" "$index"' EXIT
git show "HEAD:$exclude_file" | sed -e 's/[[:space:]]*$//' -e '/^$/d' -e '/^#/d' > "$excludes"
echo "$exclude_file" >> "$excludes"
while IFS= read -r p
do
  case "$p" in
    /* | *..*) echo "$exclude_file: '$p' must be a path inside the repository"; exit 1 ;;
  esac
done < "$excludes"
rm -f "$index"
GIT_INDEX_FILE=$index git read-tree HEAD
while IFS= read -r p
do
  GIT_INDEX_FILE=$index git rm -r -q --cached --ignore-unmatch -- "${p%/}"
done < "$excludes"
tree=$(GIT_INDEX_FILE=$index git write-tree)
# fail closed: nothing excluded may be left
while IFS= read -r p
do
  if [ -n "$(git ls-tree -r --name-only "$tree" -- "${p%/}")" ]
  then
    echo "refusing to publish: $p would still be in the public tree"
    exit 1
  fi
done < "$excludes"
echo "public tree $(git rev-parse --short "$tree"): $(git ls-tree -r --name-only "$tree" | wc -l | tr -d ' ') files," \
  "$(( $(git ls-tree -r --name-only HEAD | wc -l) - $(git ls-tree -r --name-only "$tree" | wc -l) )) left out ($(paste -sd' ' "$excludes"))"

author=$(printf '%s <%s>' "$name" "$email" | tr '[:upper:]' '[:lower:]')
while IFS= read -r p
do
  p=$(printf '%s' "$p" | tr '[:upper:]' '[:lower:]')
  case "$author" in
    *"$p"*)
      echo "refusing to publish: the public author ($name <$email>) contains something from the blocklist"
      exit 1 ;;
  esac
done < "$patterns"
if git grep -n -I -i -F -f "$patterns" "$tree" -- .
then
  echo
  echo "refusing to publish: the lines above contain something from the blocklist"
  exit 1
fi

if [ -n "$dry" ]
then
  echo "dry run: nothing pushed"
  exit 0
fi

git fetch -q "$remote" 2> /dev/null || true
# a new branch starts from main
parent=$(git rev-parse -q --verify "refs/remotes/$remote/$branch" || git rev-parse -q --verify "refs/remotes/$remote/main" || true)
commit=$(GIT_AUTHOR_NAME=$name GIT_AUTHOR_EMAIL=$email GIT_COMMITTER_NAME=$name GIT_COMMITTER_EMAIL=$email \
  git commit-tree "$tree" ${parent:+-p "$parent"} -m "$msg")
if [ -n "$parent" ] && [ -z "$(git diff --name-only "$parent" "$commit")" ]
then
  echo "nothing new to publish"
  commit=$parent
else
  git push "$remote" "$commit:refs/heads/$branch"
  echo "published $(git rev-parse --short "$commit") to $remote $branch"
fi
if [ -n "$release" ]
then
  git push "$remote" "$commit:refs/heads/release"
  echo "release branch updated: CI builds v$(tr -d '[:space:]' < VERSION) on $remote"
fi
