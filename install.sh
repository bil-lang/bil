#!/bin/sh
# Installs the bil CLI. Usage: curl -fsSL https://bil-lang.org/install.sh | sh
#
# Env overrides:
#   BIL_VERSION      release tag to install (default: latest)
#   BIL_INSTALL_DIR  directory to install the bil binary into (default: $HOME/.local/bin)
set -eu

repo="bil-lang/bil"
install_dir="${BIL_INSTALL_DIR:-$HOME/.local/bin}"
version="${BIL_VERSION:-latest}"

os="$(uname -s)"
arch="$(uname -m)"

case "$os" in
	Darwin) os_name="darwin" ;;
	Linux) os_name="linux" ;;
	*)
		echo "bil: unsupported OS: $os" >&2
		echo "bil: on native Windows, use: irm https://bil-lang.org/install.ps1 | iex" >&2
		exit 1
		;;
esac

case "$arch" in
	x86_64 | amd64) arch_name="amd64" ;;
	arm64 | aarch64) arch_name="arm64" ;;
	*)
		echo "bil: unsupported architecture: $arch" >&2
		exit 1
		;;
esac

if [ "$version" = "latest" ]; then
	version="$(curl -fsSL "https://api.github.com/repos/${repo}/releases/latest" |
		grep '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
	if [ -z "$version" ]; then
		echo "bil: couldn't resolve the latest release version" >&2
		exit 1
	fi
fi

archive="bil_${version}_${os_name}_${arch_name}.tar.gz"
checksums="bil_${version}_checksums.txt"
base_url="https://github.com/${repo}/releases/download/${version}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "bil: downloading ${archive} (${version})..."
curl -fsSL "${base_url}/${archive}" -o "${tmp}/${archive}"
curl -fsSL "${base_url}/${checksums}" -o "${tmp}/${checksums}"

echo "bil: verifying checksum..."
expected="$(grep " ${archive}\$" "${tmp}/${checksums}" | awk '{print $1}')"
if [ -z "$expected" ]; then
	echo "bil: no checksum entry found for ${archive}" >&2
	exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "${tmp}/${archive}" | awk '{print $1}')"
else
	actual="$(shasum -a 256 "${tmp}/${archive}" | awk '{print $1}')"
fi
if [ "$expected" != "$actual" ]; then
	echo "bil: checksum mismatch for ${archive}" >&2
	echo "bil: expected ${expected}, got ${actual}" >&2
	exit 1
fi

tar -xzf "${tmp}/${archive}" -C "${tmp}"

mkdir -p "$install_dir"
mv "${tmp}/bil" "${install_dir}/bil"
chmod +x "${install_dir}/bil"

echo "bil: installed to ${install_dir}/bil"

case ":${PATH}:" in
	*":${install_dir}:"*) ;;
	*)
		echo
		echo "bil: ${install_dir} is not on your PATH. Add it, e.g.:"
		echo "    echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.profile"
		;;
esac

if ! command -v go >/dev/null 2>&1; then
	echo
	echo "bil: note — 'go' was not found on PATH. 'bil run' shells out to the Go"
	echo "bil: toolchain to execute transpiled programs (Bil compiles to Go and runs"
	echo "bil: on it directly). 'bil vet' works without it. Install Go from:"
	echo "bil:   https://go.dev/dl/"
fi
