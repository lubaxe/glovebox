#!/bin/bash -eu
#
# Publishes the current commit's files to the public repository as one new
# commit, without this repository's history.
#
# usage: scripts/publish.sh [--release | --branch NAME] "commit message"
#
# --release also points the public repository's release branch at the new
# commit, which makes its CI build the image and publish release
# v<VERSION>. Bump VERSION first.
#
# --branch NAME publishes to that branch instead of main, e.g. "rc" for a
# release candidate whose image is built by running Build image by hand on
# that branch, while main and the latest release stay as they are.
#
# Setup, once:
#   git remote add public https://github.com/<owner>/glovebox.git
#   git config glovebox.publicEmail <id>+<user>@users.noreply.github.com
#   and list anything that must never be published (VIN, home addresses,
#   names, ...), one per line, in .git/info/publish-blocklist. That file is
#   inside .git, so it's never committed itself.

release=
branch=main
while [ $# -gt 0 ]
do
  case "$1" in
    --release) release=1 ;;
    --branch) branch=${2:?--branch needs a name}; shift ;;
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
name=$(git config user.name)
email=$(git config glovebox.publicEmail || true)
if [ -z "$email" ]
then
  echo "set the public commit address first: git config glovebox.publicEmail <address>"
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
if git grep -n -I -i -F -f "$patterns" HEAD -- .
then
  echo
  echo "refusing to publish: the lines above contain something from the blocklist"
  exit 1
fi

git fetch -q "$remote" 2> /dev/null || true
# a new branch starts from main
parent=$(git rev-parse -q --verify "refs/remotes/$remote/$branch" || git rev-parse -q --verify "refs/remotes/$remote/main" || true)
commit=$(GIT_AUTHOR_NAME=$name GIT_AUTHOR_EMAIL=$email GIT_COMMITTER_NAME=$name GIT_COMMITTER_EMAIL=$email \
  git commit-tree "HEAD^{tree}" ${parent:+-p "$parent"} -m "$msg")
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
