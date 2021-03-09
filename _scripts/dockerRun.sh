#!/usr/bin/env vbash

source "${BASH_SOURCE%/*}/common.bash"

# This ensures that GitHub Actions properly runs the after_failure script
trap 'set +ev' EXIT

# We run race builds/tests on main branch. We also define that the RACE_BUILD
# environment variable be a comma-separated list of PR numbers (just the
# number, no '#'), and if the CI build in question is a PR build whose number
# is present in RACE_BUILD we also run race builds/tests.
runRace=false
if [[ "${CI:-}" == "true" ]]
then
	if [[ "${GITHUB_REF:-}" == "refs/heads/main" ]]
	then
		runRace=true
	elif [[ "${GITHUB_EVENT_NAME:-}" == "pull_request" ]]
	then
		for i in $(echo ${RACE_BUILD:-} | sed "s/,/ /g")
		do
			if [[ "${GITHUB_PR_NUMBER:-}" == "$i" ]]
			then
				runRace=true
			fi
		done
	fi
fi

if [[ "$runRace" == "true" ]]
then
	echo "Will run race builds"
else
	echo "Will NOT run race builds"
fi

if [ "${VIM_COMMAND:-}" == "" ]
then
	eval "VIM_COMMAND=\"\$DEFAULT_${VIM_FLAVOR^^}_COMMAND\""
fi

cat <<EOD
Environment is:
  VIM_FLAVOR=$VIM_FLAVOR
  VIM_COMMAND=$VIM_COMMAND
EOD

if [ "${GH_USER:-}" != "" ] && [ "${GH_TOKEN:-}" != "" ]
then
	echo -e "machine api.github.com\n  login $GH_USER\n  password $GH_TOKEN" >> ~/.netrc
	echo -e "machine github.com\n  login $GH_USER\n  password $GH_TOKEN" >> ~/.netrc
	echo -e "machine githubusercontent.com\n  login $GH_USER\n  password $GH_TOKEN" >> ~/.netrc
fi

go version
vim --version

if [ "${GITHUB_EVENT_NAME:-}" == "schedule" ]
then
	go mod edit -dropreplace=golang.org/x/tools -dropreplace=golang.org/x/tools/gopls
	go get golang.org/x/tools/gopls@master golang.org/x/tools@master
	go list -m golang.org/x/tools/gopls golang.org/x/tools
fi

./_scripts/revendorToolsInternal.sh

go install golang.org/x/tools/gopls

# remove all generated files to ensure we are never stale
rm -f $(git ls-files -- ':!:cmd/govim/internal/golang_org_x_tools' '**/gen_*.*' 'gen_*.*')

# Run the install scripts
export GOVIM_RUN_INSTALL_TESTSCRIPTS=true

# Turn on gopls verbose logging by default
export GOVIM_GOPLS_VERBOSE_OUTPUT=true

# Turn on logging of raw file watcher events
export GOVIM_FILEWATCHER_DEBUG=true

go generate $(go list ./... | grep -v 'govim/internal/golang_org_x_tools')
go run ./internal/cmd/dots go test $(go list ./... | grep -v 'govim/internal/golang_org_x_tools')

if [[ "$runRace" == "true" ]]
then
	go run ./internal/cmd/dots go test -race -timeout 0s $(go list ./... | grep -v 'govim/internal/golang_org_x_tools')
fi

go vet $(go list ./... | grep -v 'govim/internal/golang_org_x_tools')
go run honnef.co/go/tools/cmd/staticcheck $(go list ./... | grep -v 'govim/internal/golang_org_x_tools')

if [ "${CI:-}" == "true" ] && [ "${GITHUB_EVENT_NAME:-}" != "schedule" ]
then
	# Hack to work around golang.org/issue/40067
	if go list -f {{context.ReleaseTags}} runtime | grep $(echo "$MAX_GO_VERSION" | sed -e 's/^\([^.]*\.[^.]*\).*/\1/') > /dev/null
	then
		go mod tidy
	fi
	diff <(echo -n) <(go run golang.org/x/tools/cmd/goimports -d $(git ls-files '**/*.go' '*.go' | grep -v golang_org_x_tools))
	test -z "$(git status --porcelain)" || (git status; git diff; false)
fi
