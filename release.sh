#!/bin/bash
show-usage () {
  cat <<EOF
Usage: $(basename $0) [options]
Options:
  -h : show this message
  -t : release version tag. (default: snapshot)
  -g : Github OAuth token. (default : GITHUB_TOKEN env var)
  -i : ignore build errors
  -b : branch(default : master)
  -s : install std
  -u : upload only
EOF
  exit 1
}

print-msg () { # print-msg level msg color
  local now=`date '+%Y/%m/%d %H:%M:%S'`
  local log=`printf "%-20s %-50s\n" "${now}" "${2}"`
  if [ ! -z "${3}" ]; then
    cRED=31; cGREEN=32; cYELLOW=33; cBLUE=34; cMAGENTA=35; cCYAN=36; cWHITE=37
    echo -e "\033[1;$(eval "echo \$c${3}")m${log}\033[0m"
  elif `echo ${1} | grep -q "W.*"` ; then
    echo -e "\033[1;33m${log}\033[0m"
  elif `echo ${1} | grep -q "E.*"` ; then
    echo -e "\033[1;31m${log}\033[0m"
  else
    echo "${log}"
  fi
  return 0
}

abort () {
  print-msg E "${1}"
  exit 1
}

handle-build-result () {
  if [ $1 -ne 0 ]; then
    if [ ${IGNORE_BUILD_ERROR} = 0 ]; then
      abort "Failed to build packages"
    else
      print-msg W "Failed to build some packages"
    fi
  else
    print-msg I "All packages have been built successfully" CYAN
  fi
}

`which greadlink >/dev/null 2&>1` && _readlink=greadlink || _readlink=readlink
SCRIPT_DIR=$(dirname $(${_readlink} -f $0))
cd "${SCRIPT_DIR}"

: ${GITHUB_TOKEN:=""}
: ${RELEASE_TAG:="snapshot"}
: ${IGNORE_BUILD_ERROR:=0}
: ${BRANCH:="master"}
: ${INSTALL_STD:=0}
: ${UPLOAD_ONLY:=0}

while : ; do
  case "${1}" in
  -*)
    [[ "$1" =~ "h" ]] && show-usage
    if [[ "$1" =~ "i" ]]; then
      IGNORE_BUILD_ERROR=1
      shift 1
    elif [[ "$1" =~ "u" ]]; then
      UPLOAD_ONLY=1
      shift 1
    elif [[ "$1" =~ "s" ]]; then
      INSTALL_STD=1
      shift 1
    elif [[ "$1" =~ "t" ]]; then
      if [[ -z "$2" || "$2" =~ "^-+" ]]; then
        echo "-t can not be empty";show-usage
      fi
      RELEASE_TAG="$2"
      shift 2
    elif [[ "$1" =~ "g" ]]; then
      if [[ -z "$2" || "$2" =~ "^-+" ]]; then
        echo "-g can not be empty";show-usage
      fi
      GITHUB_TOKEN="$2"
      shift 2
    elif [[ "$1" =~ "b" ]]; then
      if [[ -z "$2" || "$2" =~ "^-+" ]]; then
        echo "-b can not be empty";show-usage
      fi
      BRANCH="$2"
      shift 2
    fi
    ;;
  *)
    break
    ;;
  esac
done

_GO_VERSION=`go version`
[ $? -ne 0 ] && abort "'go' command not found on PATH"
if [ ! `which gox >/dev/null 2>&1` ]; then
  print-msg I "'gox' command not found on PATH."
  print-msg I "Installing gox..."
  go get github.com/mitchellh/gox
  [ $? -ne 0 ] && abort "Failed to install gox"
fi
if [ ! `which ghr >/dev/null 2>&1` ]; then
  print-msg I "'ghr' command not found on PATH."
  print-msg I "Installing ghr..."
  go get github.com/tcnksm/ghr
  [ $? -ne 0 ] && abort "Failed to install ghr"
fi

CPU_NUM="${NUMBER_OF_PROCESSORS}"
if [ -z "${CPU_NUM}" ]; then
  CPU_NUM=$(python -c 'import multiprocessing; print(multiprocessing.cpu_count())')
fi
print-msg I "num of cpus: ${CPU_NUM}"

_SUDO=sudo
if `echo "${OS}" | grep -q "Win"` ; then
  _SUDO=
fi

_OLD_IFS="${IFS}"
IFS='
'
GOX_OSARCHS=(`gox -osarch-list | grep 'true' | awk '{print $1}'`)
IFS="${_OLD_IFS}"

print-msg I "${_GO_VERSION}"
if [ ${INSTALL_STD} = 1 ]; then
  print-msg I "Install std libraries"
  IFS='
'
  for _OSARCH in "${GOX_OSARCHS[@]}" ; do
    _OS=$(echo ${_OSARCH} | sed -E 's!(.*)/(.*)!\1!g')
    _ARCH=$(echo ${_OSARCH} | sed -E 's!(.*)/(.*)!\2!g')
    print-msg I "${_SUDO} env GOOS=${_OS} GOARCH=${_ARCH} go install std"
    ${_SUDO} env PATH="${PATH}" GOROOT="${GOROOT}" GOPATH="${GOPATH}" GOOS=${_OS} GOARCH=${_ARCH} go install std
    [ $? -ne 0 ] && print-msg W "Failed to install std"
  done
fi

if [ ${UPLOAD_ONLY} -eq 0 ]; then
  print-msg I "tag: ${RELEASE_TAG}"
  _OLD_BRANCH=$(git rev-parse --abbrev-ref HEAD)
  if [ "${_OLD_BRANCH}" != "${BRANCH}" ]; then
    print-msg I "git checkout ${BRANCH}"
    git checkout ${BRANCH}
    [ $? -ne 0 ] && abort "Failed to checkout ${BRANCH}"
  fi
  
  if [ "${RELEASE_TAG}" != "snapshot" ]; then
    print-msg I git checkout refs/tags/${RELEASE_TAG}
    git checkout refs/tags/${RELEASE_TAG}
    [ $? -ne 0 ] && abort "Failed to checkout the tag ${RELEASE_TAG}"
  fi
  
  rm -rf "${SCRIPT_DIR}/packages"
  _GOX_OS="darwin windows linux freebsd"
  print-msg I "gox -output=${SCRIPT_DIR}/packages/{{.Dir}}_${RELEASE_TAG}_{{.OS}}_{{.Arch}}" -os=${_GOX_OS} -ldflags="-s"
  env CGO_ENABLED=0 gox -output="${SCRIPT_DIR}/packages/{{.Dir}}_${RELEASE_TAG}_{{.OS}}_{{.Arch}}" -os="${_GOX_OS}" -ldflags="-s"
  handle-build-result $?
fi

_NUM_THREADS=${CPU_NUM}
if [ -z "${CPU_NUM}" -o ${CPU_NUM} -lt 4 ]; then
  _NUM_THREADS=4
fi

print-msg I "ghr --parallel=${_NUM_THREADS} --delete --token=**** ${RELEASE_TAG} packages"
ghr --parallel=${_NUM_THREADS} --delete --token=${GITHUB_TOKEN} ${RELEASE_TAG} packages
[ $? -ne 0 ] && abort "Failed to upload some packages"
print-msg I "All packages have been uploaded successfully" CYAN

if [ ${UPLOAD_ONLY} -eq 0 ]; then
  if [ "${_OLD_BRANCH}" != "${BRANCH}" ]; then
    print-msg I "git checkout ${_OLD_BRANCH}"
    git checkout ${_OLD_BRANCH}
    [ $? -ne 0 ] && abort "Failed to checkout ${_OLD_BRANCH}"
  fi
fi

print-msg I "OK" CYAN
