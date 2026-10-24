#!/bin/bash
# Helpers for setting up an ONIX adapter node (the Go adapter in cmd/adapter)
# that participates in a Beckn network. Sourced by install/beckn-onix.sh.
#
# The flow is: collect inputs (onix_collect_*), confirm, generate keys,
# write a self-contained configuration directory (config/onix-node/ by
# default) containing the adapter config, its routing rules, the registry
# registration details and the docker compose environment, and finally start
# the adapter and its Redis with install/docker-compose-onix-node.yml.
#
# Everything here is bash 3.2 compatible (no associative arrays) and never
# uses eval: values are returned from prompt helpers with printf -v.
#
# Callers may override ONIX_NODE_CONFIG_DIR (host directory for the generated
# configuration) before sourcing, e.g. for testing.

ONIX_NODE_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONIX_INSTALL_DIR="$(cd "$ONIX_NODE_LIB_DIR/.." && pwd)"
ONIX_REPO_DIR="$(cd "$ONIX_INSTALL_DIR/.." && pwd)"

ONIX_NODE_CONFIG_DIR="${ONIX_NODE_CONFIG_DIR:-$ONIX_REPO_DIR/config/onix-node}"
# The compose file mounts ../config at /app/config, so config/onix-node/ on the
# host is /app/config/onix-node/ inside the adapter container.
ONIX_NODE_CONTAINER_DIR="/app/config/onix-node"
ONIX_NODE_COMPOSE_FILE="$ONIX_INSTALL_DIR/docker-compose-onix-node.yml"
ONIX_NODE_ENV_FILE_NAME="compose.env"
ONIX_NODE_REDIS_ADDR="redis-onix-node:6379"
# A node running both roles gives its BPP modules a second Redis: the payload
# store keys entries by message_id only, so a BAP and BPP sharing one Redis
# overwrite each other's records when they transact with each other (see
# SETUP.md, "Redis: One Instance per ONIX Node").
ONIX_NODE_REDIS_BPP_ADDR="redis-onix-node-bpp:6379"
ONIX_NODE_HTTP_PORT=8081
ONIX_DEDI_REGISTRY_URL="https://fabric.nfh.global/registry/dedi"
# Fallback key generator image (see onix_generate_keys).
ONIX_KEYGEN_IMAGE="golang:1.26-alpine"

# ---------------------------------------------------------------------------
# Input validation and prompt helpers
# ---------------------------------------------------------------------------

# onix_is_url VALUE - succeeds for an absolute http(s) URL with a host and no
# whitespace, quotes, backslashes or query/fragment. The value is written into
# YAML and JSON, so the character set is deliberately conservative.
onix_is_url() {
    local chars='^https?://[A-Za-z0-9._~%:@!$&()*+,;=/-]+$' host='^https?://[^/:@]+'
    [[ "$1" =~ $chars ]] && [[ "$1" =~ $host ]]
}

# onix_is_subscriber_id VALUE - subscriber ids are usually domain names
# (bap.example.com); allow the characters registries accept in practice.
onix_is_subscriber_id() {
    [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9._:/-]*$ ]]
}

# onix_is_key VALUE - a raw 32-byte key, base64 encoded (43 chars + "=").
onix_is_key() {
    [[ "$1" =~ ^[A-Za-z0-9+/]{43}=$ ]]
}

# onix_is_version VALUE - a Beckn protocol version such as 1.1.0 or 2.0.0.
onix_is_version() {
    [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
}

# onix_is_v2 VERSION - succeeds when VERSION is 2.x.x or later; the adapter's
# router treats those as domain-agnostic.
onix_is_v2() {
    local major=${1%%.*}
    [[ "$major" =~ ^[0-9]+$ ]] && ((major >= 2))
}

# onix_read VAR PROMPT [secret] - read one line into VAR. With "secret" the
# answer is not echoed (private keys, passwords). The installer stops when the
# input ends instead of re-prompting forever.
onix_read() {
    local __line
    if [[ "${3:-}" == "secret" ]]; then
        read -r -s -p "$2" __line || [[ -n "$__line" ]] || onix_input_ended
        echo ""
    else
        read -r -p "$2" __line || [[ -n "$__line" ]] || onix_input_ended
    fi
    printf -v "$1" '%s' "$__line"
}

onix_input_ended() {
    echo ""
    echo "${RED}Input ended; stopping the installer.${NC}" >&2
    exit 1
}

# onix_prompt VAR PROMPT [DEFAULT] [secret] - read a single line into VAR,
# falling back to DEFAULT when the answer is empty.
onix_prompt() {
    local __var=$1 __prompt=$2 __default=${3:-} __answer
    if [[ -n "$__default" ]]; then
        onix_read __answer "$__prompt [$__default]: " "${4:-}"
    else
        onix_read __answer "$__prompt: " "${4:-}"
    fi
    # Trim surrounding whitespace.
    __answer="${__answer#"${__answer%%[![:space:]]*}"}"
    __answer="${__answer%"${__answer##*[![:space:]]}"}"
    [[ -z "$__answer" ]] && __answer=$__default
    printf -v "$__var" '%s' "$__answer"
}

# onix_prompt_url VAR PROMPT [DEFAULT] [optional] - prompt until a valid
# http(s) URL is entered. A trailing slash is removed. With a fourth argument
# of "optional" an empty answer is accepted.
onix_prompt_url() {
    local __var=$1 __prompt=$2 __default=${3:-} __optional=${4:-} __value
    while true; do
        onix_prompt __value "$__prompt" "$__default"
        if [[ -z "$__value" && "$__optional" == "optional" ]]; then
            break
        fi
        __value="${__value%/}"
        if onix_is_url "$__value"; then
            break
        fi
        echo "${RED}Invalid URL. Enter an absolute URL starting with http:// or https:// (no spaces or quotes).${NC}"
    done
    printf -v "$__var" '%s' "$__value"
}

# onix_prompt_subscriber_id VAR PROMPT [DEFAULT]
onix_prompt_subscriber_id() {
    local __var=$1 __prompt=$2 __default=${3:-} __value
    while true; do
        onix_prompt __value "$__prompt" "$__default"
        if onix_is_subscriber_id "$__value"; then
            break
        fi
        echo "${RED}Invalid value. Use letters, digits and . _ : / - only (e.g. bap.example.com).${NC}"
    done
    printf -v "$__var" '%s' "$__value"
}

# onix_prompt_key VAR PROMPT [secret] - prompt for a base64 key ("secret" for
# private keys, which are then not echoed). A 64-byte Ed25519 private key (seed
# + public key, as written by the legacy protocol server) is accepted and
# reduced to its 32-byte seed.
onix_prompt_key() {
    local __var=$1 __prompt=$2 __mode=${3:-} __value __seed
    while true; do
        onix_prompt __value "$__prompt" "" "$__mode"
        if onix_is_key "$__value"; then
            break
        fi
        if [[ "$__value" =~ ^[A-Za-z0-9+/]{86}==$ ]]; then
            __seed=$(printf '%s' "$__value" | onix_base64_decode | head -c 32 | base64 | tr -d '\n')
            if onix_is_key "$__seed"; then
                echo "${YELLOW}Using the 32-byte seed of the 64-byte private key you entered.${NC}"
                __value=$__seed
                break
            fi
        fi
        echo "${RED}Invalid key. Expected a base64-encoded 32-byte key (44 characters ending in '=').${NC}"
    done
    printf -v "$__var" '%s' "$__value"
}

# onix_prompt_yes_no PROMPT DEFAULT(Y|N) - succeeds on yes.
onix_prompt_yes_no() {
    local __prompt=$1 __default=$2 __answer
    while true; do
        if [[ "$__default" == "Y" ]]; then
            onix_read __answer "$__prompt [Y/n]: "
        else
            onix_read __answer "$__prompt [y/N]: "
        fi
        [[ -z "$__answer" ]] && __answer=$__default
        case "$__answer" in
        Y | y | yes | YES | Yes) return 0 ;;
        N | n | no | NO | No) return 1 ;;
        esac
        echo "${RED}Please answer y or n.${NC}"
    done
}

# onix_prompt_choice VAR PROMPT MAX [DEFAULT] - prompt until a number in 1..MAX.
onix_prompt_choice() {
    local __var=$1 __prompt=$2 __max=$3 __default=${4:-} __value
    while true; do
        onix_prompt __value "$__prompt" "$__default"
        if [[ "$__value" =~ ^[0-9]+$ ]] && ((__value >= 1 && __value <= __max)); then
            break
        fi
        echo "${RED}Invalid input. Please enter a number between 1 and $__max.${NC}"
    done
    printf -v "$__var" '%s' "$__value"
}

onix_base64_decode() {
    base64 -d 2>/dev/null || base64 -D
}

# onix_public_key_for ALG(ed25519|x25519) PRIVATE - derive the base64 public
# key of a raw base64 private key with OpenSSL by wrapping it in its PKCS#8
# DER encoding. Prints nothing when OpenSSL cannot do it.
onix_public_key_for() {
    local prefix
    command -v openssl >/dev/null 2>&1 || return 0
    case $1 in
    ed25519) prefix='\x30\x2e\x02\x01\x00\x30\x05\x06\x03\x2b\x65\x70\x04\x22\x04\x20' ;;
    x25519) prefix='\x30\x2e\x02\x01\x00\x30\x05\x06\x03\x2b\x65\x6e\x04\x22\x04\x20' ;;
    *) return 0 ;;
    esac
    { printf '%b' "$prefix" && printf '%s' "$2" | onix_base64_decode; } 2>/dev/null |
        openssl pkey -inform DER -pubout -outform DER 2>/dev/null | tail -c 32 | base64 | tr -d '\n'
}

# onix_key_pair_matches ALG PRIVATE PUBLIC - fails only when the public key
# provably does not belong to the private key.
onix_key_pair_matches() {
    local derived
    derived=$(onix_public_key_for "$1" "$2")
    ! onix_is_key "$derived" || [[ "$derived" == "$3" ]]
}

# ---------------------------------------------------------------------------
# Node state
#
# A node has one role (BAP, BPP or BOTH) and takes part in one or more
# networks. Per-network values live in indexed arrays so the same generator
# serves a single network (prefix "") and several networks (prefix "/<name>").
# ---------------------------------------------------------------------------

onix_reset_state() {
    onix_role=""
    onix_replace_confirmed=0
    onix_public_base_url=""
    onix_bap_app_url=""
    onix_bpp_app_url=""
    onix_log_level="info"
    onix_image="fidedocker/onix-adapter"
    onix_host_port="8081"
    onix_bind_address="127.0.0.1"

    onix_net_count=0
    onix_net_name=()
    onix_net_prefix=()
    onix_net_registry_type=()
    onix_net_registry_url=()
    onix_net_network_ids=()
    onix_net_version=()
    onix_net_domain=()
    onix_net_search_url=()

    onix_bap_id=()
    onix_bap_url=()
    onix_bap_key_id=()
    onix_bap_key_mode=()
    onix_bap_sign_priv=()
    onix_bap_sign_pub=()
    onix_bap_encr_priv=()
    onix_bap_encr_pub=()

    onix_bpp_id=()
    onix_bpp_url=()
    onix_bpp_key_id=()
    onix_bpp_key_mode=()
    onix_bpp_sign_priv=()
    onix_bpp_sign_pub=()
    onix_bpp_encr_priv=()
    onix_bpp_encr_pub=()
}

# onix_role_is ROLE - succeeds when the collected role is ROLE.
onix_role_is() {
    [[ "$onix_role" == "$1" ]]
}

onix_has_bap() {
    [[ "$onix_role" == "BAP" || "$onix_role" == "BOTH" ]]
}

onix_has_bpp() {
    [[ "$onix_role" == "BPP" || "$onix_role" == "BOTH" ]]
}

# ---------------------------------------------------------------------------
# Input collection
# ---------------------------------------------------------------------------

# onix_collect_role [with_gateway] - sets onix_role to BAP, BPP or BOTH, or
# GATEWAY when "with_gateway" is passed and the legacy gateway is chosen.
onix_collect_role() {
    local choice max=3
    echo -e "\nWhich role will this node play in the network?"
    echo "1. BAP (buyer application platform) via ONIX adapter"
    echo "2. BPP (provider platform) via ONIX adapter"
    echo "3. Both BAP and BPP on one ONIX adapter"
    if [[ "${1:-}" == "with_gateway" ]]; then
        echo "4. Beckn Gateway (legacy gateway service, not an ONIX adapter)"
        max=4
    fi
    onix_prompt_choice choice "Enter your choice" "$max"
    case $choice in
    1) onix_role="BAP" ;;
    2) onix_role="BPP" ;;
    3) onix_role="BOTH" ;;
    4) onix_role="GATEWAY" ;;
    esac
}

# onix_collect_network INDEX NAME PREFIX - registry and protocol details of
# one network.
onix_collect_network() {
    local i=$1 choice url ids version domain search_url default_version
    onix_net_name[i]=$2
    ((i + 1 > onix_net_count)) && onix_net_count=$((i + 1))
    onix_net_prefix[i]=$3

    echo -e "\nWhich registry does the network use?"
    echo "1. Beckn One / DeDi registry (dediregistry plugin)"
    echo "2. Registry with a /lookup API, e.g. the Beckn registry this installer deploys (registry plugin)"
    onix_prompt_choice choice "Enter your choice" 2
    if [[ $choice -eq 1 ]]; then
        onix_net_registry_type[i]="dedi"
        echo "${BLUE}The DeDi registry URL is a locked Beckn constant ($ONIX_DEDI_REGISTRY_URL); it is not written to the config.${NC}"
        while true; do
            onix_prompt ids "Network ID(s) to accept participants from, comma-separated (e.g. beckn.one/testnet; empty accepts any network)"
            ids=$(printf '%s' "$ids" | tr -d '[:space:]')
            if [[ -z "$ids" || "$ids" =~ ^[A-Za-z0-9._:/-]+(,[A-Za-z0-9._:/-]+)*$ ]]; then
                break
            fi
            echo "${RED}Invalid network ID list. Use comma-separated IDs such as beckn.one/testnet.${NC}"
        done
        if [[ -z "$ids" ]]; then
            echo "${YELLOW}Warning: participants of every Beckn One network will be accepted; enter network IDs to restrict this.${NC}"
        fi
        onix_net_network_ids[i]=$ids
        onix_net_registry_url[i]=""
        default_version="2.0.0"
    else
        onix_net_registry_type[i]="legacy"
        echo "${BLUE}The adapter calls <registry URL>/lookup, so include the path prefix of the subscriber API.${NC}"
        onix_prompt_url url "Registry URL (e.g. https://registry.example.com/subscribers)"
        onix_net_registry_url[i]=$url
        onix_net_network_ids[i]=""
        default_version="1.1.0"
    fi

    while true; do
        onix_prompt version "Beckn protocol version used by the network" "$default_version"
        if onix_is_version "$version"; then
            break
        fi
        echo "${RED}Invalid version. Use the form MAJOR.MINOR.PATCH, e.g. 1.1.0 or 2.0.0.${NC}"
    done
    onix_net_version[i]=$version

    domain=""
    if ! onix_is_v2 "$version"; then
        # Beckn 1.x routing rules are keyed by domain.
        while true; do
            onix_prompt domain "Network domain (e.g. retail:1.1.0, ONDC:TRV10)"
            if [[ "$domain" =~ ^[A-Za-z0-9._:-]+$ ]]; then
                break
            fi
            echo "${RED}Invalid domain. Use letters, digits and . _ : - only.${NC}"
        done
    fi
    onix_net_domain[i]=$domain

    search_url=""
    if onix_has_bap; then
        if onix_is_v2 "$version"; then
            onix_prompt_url search_url "Discovery service URL for 'discover' calls (empty sends discover to the bpp_uri in the request)" "" optional
        else
            onix_prompt_url search_url "Gateway URL for 'search' calls, e.g. https://gateway.example.com/bg (empty sends search to the bpp_uri in the request)" "" optional
        fi
    fi
    onix_net_search_url[i]=$search_url
}

# onix_collect_participant INDEX ROLE(BAP|BPP) - subscriber identity and keys
# of this node in network INDEX.
onix_collect_participant() {
    local i=$1 role=$2 lrole id url key_id choice max sp spub ep epub mode
    local path prefix=${onix_net_prefix[$1]}
    lrole=$(printf '%s' "$role" | tr '[:upper:]' '[:lower:]')
    path="$prefix/$lrole/receiver"

    echo -e "\n${GREEN}$role subscriber details${NC}${onix_net_name[i]:+ for network ${onix_net_name[i]}}"
    onix_prompt_subscriber_id id "$role subscriber ID (e.g. $lrole.example.com)"
    onix_prompt_url url "$role subscriber URL registered in the registry" "$onix_public_base_url$path"
    onix_prompt_subscriber_id key_id "Key ID registered for this subscriber (unique_key_id / DeDi record ID)" "$id-key"

    echo "How should the signing and encryption keys for $id be set up?"
    echo "1. Generate new keys (register the public keys with the registry)"
    echo "2. Enter existing keys already registered for this subscriber"
    max=2
    if ((i > 0)); then
        echo "3. Reuse the keys of network '${onix_net_name[0]}'"
        max=3
    fi
    onix_prompt_choice choice "Enter your choice" "$max" 1

    sp="" spub="" ep="" epub=""
    case $choice in
    1) mode="generate" ;;
    2)
        mode="existing"
        while true; do
            onix_prompt_key sp "Signing private key (base64 Ed25519 seed; not shown)" secret
            onix_prompt_key spub "Signing public key (base64)"
            onix_key_pair_matches ed25519 "$sp" "$spub" && break
            echo "${RED}The signing public key does not belong to the signing private key. Enter both again.${NC}"
        done
        echo "${BLUE}Leave the encryption private key empty to generate a new X25519 pair.${NC}"
        while true; do
            onix_prompt ep "Encryption private key (base64 X25519, optional; not shown)" "" secret
            [[ -z "$ep" ]] && break
            if ! onix_is_key "$ep"; then
                echo "${RED}Invalid key. Expected a base64-encoded 32-byte key.${NC}"
                continue
            fi
            onix_prompt_key epub "Encryption public key (base64 X25519)"
            onix_key_pair_matches x25519 "$ep" "$epub" && break
            echo "${RED}The encryption public key does not belong to the encryption private key. Enter both again.${NC}"
        done
        ;;
    3) mode="reuse" ;;
    esac

    if [[ "$role" == "BAP" ]]; then
        onix_bap_id[i]=$id
        onix_bap_url[i]=$url
        onix_bap_key_id[i]=$key_id
        onix_bap_key_mode[i]=$mode
        onix_bap_sign_priv[i]=$sp
        onix_bap_sign_pub[i]=$spub
        onix_bap_encr_priv[i]=$ep
        onix_bap_encr_pub[i]=$epub
    else
        onix_bpp_id[i]=$id
        onix_bpp_url[i]=$url
        onix_bpp_key_id[i]=$key_id
        onix_bpp_key_mode[i]=$mode
        onix_bpp_sign_priv[i]=$sp
        onix_bpp_sign_pub[i]=$spub
        onix_bpp_encr_priv[i]=$ep
        onix_bpp_encr_pub[i]=$epub
    fi
}

# onix_collect_public_base_url - the URL other participants use to reach this
# adapter (normally a TLS-terminating reverse proxy in front of it).
onix_collect_public_base_url() {
    echo -e "\nOther participants must reach this adapter's receiver endpoints over the internet,"
    echo "normally through a reverse proxy / load balancer that terminates TLS."
    onix_prompt_url onix_public_base_url "Public base URL of this adapter (e.g. https://onix.example.com)"
    if [[ "$onix_public_base_url" == http://* ]]; then
        echo "${YELLOW}Warning: the public URL is not HTTPS. Production networks expect TLS on participant URLs.${NC}"
    fi
}

# onix_collect_app_urls - where the adapter delivers inbound messages.
onix_collect_app_urls() {
    echo -e "\nThe adapter forwards inbound messages to your application; the action name is appended to the URL."
    if onix_has_bap; then
        onix_prompt_url onix_bap_app_url "URL of your BAP application for callbacks (e.g. http://bap-app:3000/beckn -> /beckn/on_select)"
    fi
    if onix_has_bpp; then
        onix_prompt_url onix_bpp_app_url "URL of your BPP application for requests (e.g. http://bpp-app:3000/beckn -> /beckn/select)"
    fi
}

# onix_collect_runtime - container image and port binding.
onix_collect_runtime() {
    echo ""
    while true; do
        onix_prompt onix_image "ONIX adapter Docker image" "$onix_image"
        # Written unquoted to compose.env, so only image-reference characters.
        if [[ "$onix_image" =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]]; then
            break
        fi
        echo "${RED}Invalid image reference, e.g. fidedocker/onix-adapter or registry.example.com/onix:1.0.${NC}"
        onix_image="fidedocker/onix-adapter"
    done
    while true; do
        onix_prompt onix_host_port "Host port for the adapter" "$onix_host_port"
        if [[ "$onix_host_port" =~ ^[0-9]+$ ]] && ((onix_host_port >= 1 && onix_host_port <= 65535)); then
            break
        fi
        echo "${RED}Invalid port.${NC}"
    done
    echo "The caller endpoints (/bap/caller/, /bpp/caller/) sign whatever they receive and must not be public."
    if onix_prompt_yes_no "Publish the adapter port on 127.0.0.1 only (recommended when a reverse proxy runs on this host)?" Y; then
        onix_bind_address="127.0.0.1"
    else
        onix_bind_address="0.0.0.0"
        echo "${YELLOW}Make sure a firewall or proxy keeps the caller endpoints private.${NC}"
    fi
}

# onix_print_summary - what will be set up, before anything is changed.
onix_print_summary() {
    local i role id key_mode
    echo -e "\n${BLUE}Summary${NC}"
    echo "  Role            : $onix_role"
    echo "  Public base URL : $onix_public_base_url"
    if onix_has_bap; then echo "  BAP app URL     : $onix_bap_app_url"; fi
    if onix_has_bpp; then echo "  BPP app URL     : $onix_bpp_app_url"; fi
    echo "  Adapter image   : $onix_image (published on $onix_bind_address:$onix_host_port)"
    echo "  Config dir      : $ONIX_NODE_CONFIG_DIR"
    for ((i = 0; i < onix_net_count; i++)); do
        echo "  Network ${onix_net_name[i]:-$((i + 1))}${onix_net_prefix[i]:+ (paths under ${onix_net_prefix[i]}/)}"
        if [[ "${onix_net_registry_type[i]}" == "dedi" ]]; then
            echo "    Registry      : Beckn One / DeDi${onix_net_network_ids[i]:+ (networks: ${onix_net_network_ids[i]})}"
        else
            echo "    Registry      : ${onix_net_registry_url[i]}"
        fi
        echo "    Version       : ${onix_net_version[i]}${onix_net_domain[i]:+, domain ${onix_net_domain[i]}}"
        [[ -n "${onix_net_search_url[i]}" ]] && echo "    Search target : ${onix_net_search_url[i]}"
        for role in BAP BPP; do
            if [[ "$role" == "BAP" ]]; then
                onix_has_bap || continue
                id=${onix_bap_id[i]} key_mode=${onix_bap_key_mode[i]}
            else
                onix_has_bpp || continue
                id=${onix_bpp_id[i]} key_mode=${onix_bpp_key_mode[i]}
            fi
            echo "    $role           : $id (keys: $key_mode)"
        done
    done
}

# ---------------------------------------------------------------------------
# Keys
# ---------------------------------------------------------------------------

# onix_generate_keys_openssl - Ed25519 and X25519 key pairs via OpenSSL 1.1.1+.
# PKCS#8 / SPKI DER encodings of these keys end in the 32 raw key bytes.
onix_generate_keys_openssl() {
    command -v openssl >/dev/null 2>&1 || return 1
    local tmp alg prefix der_len rc=1
    tmp=$(mktemp -d) || return 1
    chmod 700 "$tmp"
    for alg in ed25519 x25519; do
        openssl genpkey -algorithm "$alg" -out "$tmp/$alg.pem" >/dev/null 2>&1 || break
        openssl pkey -in "$tmp/$alg.pem" -outform DER -out "$tmp/$alg.der" >/dev/null 2>&1 || break
        openssl pkey -in "$tmp/$alg.pem" -pubout -outform DER -out "$tmp/$alg.pub.der" >/dev/null 2>&1 || break
        der_len=$(wc -c <"$tmp/$alg.der" | tr -d ' ')
        [[ "$der_len" == "48" ]] || break
        der_len=$(wc -c <"$tmp/$alg.pub.der" | tr -d ' ')
        [[ "$der_len" == "44" ]] || break
        prefix=sign
        [[ "$alg" == "x25519" ]] && prefix=encr
        printf -v "onix_gen_${prefix}_priv" '%s' "$(tail -c 32 "$tmp/$alg.der" | base64 | tr -d '\n')"
        printf -v "onix_gen_${prefix}_pub" '%s' "$(tail -c 32 "$tmp/$alg.pub.der" | base64 | tr -d '\n')"
        [[ "$alg" == "x25519" ]] && rc=0
    done
    rm -rf "$tmp"
    return $rc
}

# onix_parse_generated_keys OUTPUT - parse install/generate-ed25519-keys.go output.
onix_parse_generated_keys() {
    onix_gen_sign_priv=$(printf '%s\n' "$1" | awk '/^signingPrivateKey:/ {print $2}')
    onix_gen_sign_pub=$(printf '%s\n' "$1" | awk '/^signingPublicKey:/ {print $2}')
    onix_gen_encr_priv=$(printf '%s\n' "$1" | awk '/^encrPrivateKey:/ {print $2}')
    onix_gen_encr_pub=$(printf '%s\n' "$1" | awk '/^encrPublicKey:/ {print $2}')
}

# onix_generated_keys_valid - succeeds when all four onix_gen_* keys are set.
onix_generated_keys_valid() {
    onix_is_key "$onix_gen_sign_priv" && onix_is_key "$onix_gen_sign_pub" &&
        onix_is_key "$onix_gen_encr_priv" && onix_is_key "$onix_gen_encr_pub"
}

# onix_generate_keys - sets onix_gen_{sign,encr}_{priv,pub}. Tries OpenSSL,
# then a local Go toolchain, then Go in a throwaway Docker container.
onix_generate_keys() {
    onix_gen_sign_priv="" onix_gen_sign_pub="" onix_gen_encr_priv="" onix_gen_encr_pub=""
    onix_generate_keys_openssl && return 0
    if command -v go >/dev/null 2>&1; then
        onix_parse_generated_keys "$(cd "$ONIX_REPO_DIR" && go run ./install/generate-ed25519-keys.go 2>/dev/null)"
        onix_generated_keys_valid && return 0
    fi
    if command -v docker >/dev/null 2>&1; then
        echo "${YELLOW}OpenSSL with Ed25519/X25519 support and a working Go toolchain were not found; generating keys with the $ONIX_KEYGEN_IMAGE image.${NC}" >&2
        # Remember to remove the image afterwards (onix_resolve_all_keys) if it is pulled here.
        if [[ -z "${onix_keygen_image_pulled:-}" ]] && ! docker image inspect "$ONIX_KEYGEN_IMAGE" >/dev/null 2>&1; then
            onix_keygen_image_pulled=1
        fi
        onix_parse_generated_keys "$(docker run --rm -v "$ONIX_INSTALL_DIR/generate-ed25519-keys.go:/keygen/main.go:ro" -w /keygen "$ONIX_KEYGEN_IMAGE" go run main.go 2>/dev/null)"
        onix_generated_keys_valid && return 0
    fi
    echo "${RED}Error: could not generate keys. Install OpenSSL 1.1.1+ or Go, or enter existing keys.${NC}" >&2
    return 1
}

# onix_resolve_participant_keys INDEX ROLE - materialise the keys chosen in
# onix_collect_participant (generate, reuse or fill in missing encryption keys).
onix_resolve_participant_keys() {
    local i=$1 role=$2 mode sp spub ep epub
    if [[ "$role" == "BAP" ]]; then
        mode=${onix_bap_key_mode[i]} sp=${onix_bap_sign_priv[i]} spub=${onix_bap_sign_pub[i]}
        ep=${onix_bap_encr_priv[i]} epub=${onix_bap_encr_pub[i]}
    else
        mode=${onix_bpp_key_mode[i]} sp=${onix_bpp_sign_priv[i]} spub=${onix_bpp_sign_pub[i]}
        ep=${onix_bpp_encr_priv[i]} epub=${onix_bpp_encr_pub[i]}
    fi

    case $mode in
    generate)
        onix_generate_keys || return 1
        sp=$onix_gen_sign_priv spub=$onix_gen_sign_pub ep=$onix_gen_encr_priv epub=$onix_gen_encr_pub
        ;;
    existing)
        if [[ -z "$ep" ]]; then
            onix_generate_keys || return 1
            ep=$onix_gen_encr_priv epub=$onix_gen_encr_pub
            echo "${YELLOW}Generated a new X25519 encryption key pair for $role in network $((i + 1)); update encr_public_key in the registry if it differs.${NC}"
        fi
        ;;
    reuse)
        if [[ "$role" == "BAP" ]]; then
            sp=${onix_bap_sign_priv[0]} spub=${onix_bap_sign_pub[0]} ep=${onix_bap_encr_priv[0]} epub=${onix_bap_encr_pub[0]}
        else
            sp=${onix_bpp_sign_priv[0]} spub=${onix_bpp_sign_pub[0]} ep=${onix_bpp_encr_priv[0]} epub=${onix_bpp_encr_pub[0]}
        fi
        ;;
    esac

    if [[ "$role" == "BAP" ]]; then
        onix_bap_sign_priv[i]=$sp onix_bap_sign_pub[i]=$spub onix_bap_encr_priv[i]=$ep onix_bap_encr_pub[i]=$epub
    else
        onix_bpp_sign_priv[i]=$sp onix_bpp_sign_pub[i]=$spub onix_bpp_encr_priv[i]=$ep onix_bpp_encr_pub[i]=$epub
    fi
}

# onix_resolve_all_keys - resolve keys for every participant of every network,
# in network order so "reuse" can copy the keys of network 1.
onix_resolve_all_keys() {
    local i rc=0
    onix_keygen_image_pulled=""
    for ((i = 0; i < onix_net_count; i++)); do
        if onix_has_bap; then onix_resolve_participant_keys "$i" BAP || { rc=1 && break; }; fi
        if onix_has_bpp; then onix_resolve_participant_keys "$i" BPP || { rc=1 && break; }; fi
    done
    # Like the busybox helper image in update_registry_details, do not leave the
    # key generation image behind.
    if [[ -n "$onix_keygen_image_pulled" ]]; then
        docker rmi "$ONIX_KEYGEN_IMAGE" >/dev/null 2>&1
    fi
    return $rc
}

# ---------------------------------------------------------------------------
# Config generation
# ---------------------------------------------------------------------------

# onix_module_name INDEX BASE - e.g. bapTxnReceiver or bapTxnReceiver-netA.
onix_module_name() {
    if [[ -n "${onix_net_prefix[$1]}" ]]; then
        printf '%s-%s' "$2" "${onix_net_name[$1]}"
    else
        printf '%s' "$2"
    fi
}

# onix_routing_rule VERSION DOMAIN TARGET_TYPE TARGET_URL ENDPOINT...
onix_routing_rule() {
    local version=$1 domain=$2 target_type=$3 target_url=$4 endpoint
    shift 4
    if [[ -n "$domain" ]]; then
        echo "  - domain: \"$domain\""
        echo "    version: \"$version\""
    else
        echo "  - version: \"$version\""
    fi
    echo "    targetType: \"$target_type\""
    if [[ -n "$target_url" ]]; then
        echo "    target:"
        echo "      url: \"$target_url\""
    fi
    echo "    endpoints:"
    for endpoint in "$@"; do
        echo "      - $endpoint"
    done
}

# onix_emit_routing INDEX ROLE KIND - routing rules for one module.
# KIND is Caller (outbound from your app) or Receiver (inbound from the network).
onix_emit_routing() {
    local i=$1 role=$2 kind=$3 version domain search_ep
    local txn="select init confirm status track cancel update rating support"
    version=${onix_net_version[i]}
    domain=${onix_net_domain[i]}
    search_ep=search
    onix_is_v2 "$version" && search_ep=discover

    echo "# Generated by install/beckn-onix.sh for $(onix_module_name "$i" "$(printf '%s' "$role" | tr '[:upper:]' '[:lower:]')Txn$kind")"
    echo "routingRules:"
    # shellcheck disable=SC2086,SC2046 # $txn and the on_* lists are deliberate word lists
    case "$role-$kind" in
    BAP-Caller)
        # Search/discover goes to the gateway or discovery service when one is
        # configured (used only if the request has no bpp_uri); the rest of the
        # transaction goes to the bpp_uri in the request.
        onix_routing_rule "$version" "$domain" bpp "${onix_net_search_url[i]}" "$search_ep"
        onix_routing_rule "$version" "$domain" bpp "" $txn
        ;;
    BAP-Receiver)
        onix_routing_rule "$version" "$domain" url "$onix_bap_app_url" "on_$search_ep" $(for a in $txn; do printf 'on_%s ' "$a"; done)
        ;;
    BPP-Receiver)
        if onix_is_v2 "$version"; then
            # In Beckn 2.x discovery is served by the discovery service, not the BPP.
            onix_routing_rule "$version" "$domain" url "$onix_bpp_app_url" $txn
        else
            onix_routing_rule "$version" "$domain" url "$onix_bpp_app_url" search $txn
        fi
        ;;
    BPP-Caller)
        if onix_is_v2 "$version"; then
            onix_routing_rule "$version" "$domain" bap "" $(for a in $txn; do printf 'on_%s ' "$a"; done)
        else
            onix_routing_rule "$version" "$domain" bap "" on_search $(for a in $txn; do printf 'on_%s ' "$a"; done)
        fi
        ;;
    esac
}

# onix_emit_module INDEX ROLE KIND - one adapter module.
#
# The plugin pipelines follow the maintained reference configs: Beckn 1.x
# networks use the config/local-simple.yaml pipeline; Beckn 2.x networks use
# the config/local-beckn-one-*.yaml pipeline (schema validation against the
# Beckn 2.0 spec, payload store for request-signature binding and ACK
# signature validation). checkPolicy is left out because it needs a
# network-specific policy file.
onix_emit_module() {
    local i=$1 role=$2 kind=$3 lrole name sub_id key_id sp spub ep epub v2=0
    local redis_addr=$ONIX_NODE_REDIS_ADDR
    lrole=$(printf '%s' "$role" | tr '[:upper:]' '[:lower:]')
    name=$(onix_module_name "$i" "${lrole}Txn$kind")
    onix_is_v2 "${onix_net_version[i]}" && v2=1
    if [[ "$role" == "BAP" ]]; then
        sub_id=${onix_bap_id[i]} key_id=${onix_bap_key_id[i]}
        sp=${onix_bap_sign_priv[i]} spub=${onix_bap_sign_pub[i]} ep=${onix_bap_encr_priv[i]} epub=${onix_bap_encr_pub[i]}
    else
        sub_id=${onix_bpp_id[i]} key_id=${onix_bpp_key_id[i]}
        sp=${onix_bpp_sign_priv[i]} spub=${onix_bpp_sign_pub[i]} ep=${onix_bpp_encr_priv[i]} epub=${onix_bpp_encr_pub[i]}
        [[ "$onix_role" == "BOTH" ]] && redis_addr=$ONIX_NODE_REDIS_BPP_ADDR
    fi

    cat <<EOF
  - name: $name
    path: ${onix_net_prefix[i]}/$lrole/$(printf '%s' "$kind" | tr '[:upper:]' '[:lower:]')/
    handler:
      type: std
      role: $lrole
      subscriberId: "$sub_id"
      httpClientConfig:
        maxIdleConns: 1000
        maxIdleConnsPerHost: 200
        idleConnTimeout: 300s
        responseHeaderTimeout: 5s
      plugins:
EOF
    if [[ "${onix_net_registry_type[i]}" == "dedi" ]]; then
        cat <<EOF
        registry:
          id: dediregistry
          config:
EOF
        if [[ -n "${onix_net_network_ids[i]}" ]]; then
            echo "            allowedNetworkIDs: \"${onix_net_network_ids[i]}\""
        fi
        cat <<EOF
            timeout: 10
            retry_max: 3
            retry_wait_min: 100ms
            retry_wait_max: 500ms
EOF
    else
        cat <<EOF
        registry:
          id: registry
          config:
            url: "${onix_net_registry_url[i]}"
            retry_max: 3
            retry_wait_min: 100ms
            retry_wait_max: 500ms
EOF
    fi
    cat <<EOF
        keyManager:
          id: simplekeymanager
          config:
            subscriberId: "$sub_id"
            keyId: "$key_id"
            signingPrivateKey: "$sp"
            signingPublicKey: "$spub"
            encrPrivateKey: "$ep"
            encrPublicKey: "$epub"
        cache:
          id: cache
          config:
            addr: $redis_addr
        signer:
          id: signer
        signValidator:
          id: signvalidator
        router:
          id: router
          config:
            routingConfig: $ONIX_NODE_CONTAINER_DIR/routing-$name.yaml
EOF
    if ((v2)); then
        cat <<EOF
        schemaValidator:
          id: schemav2validator
          config:
            cacheTTL: "3600"
        payloadStore:
          id: payloadstore
          config:
            ttl: "24h"
            indexTTL: "25h"
            maxBodyBytes: "1048576"
            storeBody: "true"
            storeSignature: "true"
            compress: "true"
EOF
    fi
    cat <<EOF
        middleware:
          - id: reqpreprocessor
            config:
              contextKeys: transaction_id,message_id
              role: $lrole
      steps:
EOF
    case "$kind-$v2" in
    Receiver-0) printf '        - %s\n' validateSign addRoute signAck ;;
    Caller-0) printf '        - %s\n' addRoute sign ;;
    Receiver-1) printf '        - %s\n' validateSign validateSchema addRoute storePayload signAck ;;
    Caller-1) printf '        - %s\n' addRoute sign validateSchema storePayload validateAckSign ;;
    esac
}

# onix_emit_adapter_config - the complete adapter configuration.
onix_emit_adapter_config() {
    local i
    cat <<EOF
# Generated by install/beckn-onix.sh on $(date -u +"%Y-%m-%dT%H:%M:%SZ").
# Contains private keys: keep this file private. See CONFIG.md for every option.
appName: "onix-node"
log:
  level: $onix_log_level
  destinations:
    - type: stdout
  contextKeys:
    - transaction_id
    - message_id
    - subscriber_id
    - module_id
http:
  port: $ONIX_NODE_HTTP_PORT
  timeout:
    read: 30
    write: 30
    idle: 30
pluginManager:
  root: ./plugins
modules:
EOF
    for ((i = 0; i < onix_net_count; i++)); do
        if onix_has_bap; then
            onix_emit_module "$i" BAP Receiver
            onix_emit_module "$i" BAP Caller
        fi
        if onix_has_bpp; then
            onix_emit_module "$i" BPP Receiver
            onix_emit_module "$i" BPP Caller
        fi
    done
}

# onix_valid_dates - sets onix_valid_from / onix_valid_until (GNU and BSD date).
onix_valid_dates() {
    if date -u -d "-1 day" +%Y >/dev/null 2>&1; then
        onix_valid_from=$(date -u -d "-1 day" +"%Y-%m-%dT%H:%M:%S.000Z")
        onix_valid_until=$(date -u -d "+3 year" +"%Y-%m-%dT%H:%M:%S.000Z")
    else
        onix_valid_from=$(date -u -v-1d +"%Y-%m-%dT%H:%M:%S.000Z")
        onix_valid_until=$(date -u -v+3y +"%Y-%m-%dT%H:%M:%S.000Z")
    fi
}

# onix_emit_participant_info INDEX ROLE - registration details of one subscriber.
onix_emit_participant_info() {
    local i=$1 role=$2 id url key_id spub epub
    if [[ "$role" == "BAP" ]]; then
        id=${onix_bap_id[i]} url=${onix_bap_url[i]} key_id=${onix_bap_key_id[i]} spub=${onix_bap_sign_pub[i]} epub=${onix_bap_encr_pub[i]}
    else
        id=${onix_bpp_id[i]} url=${onix_bpp_url[i]} key_id=${onix_bpp_key_id[i]} spub=${onix_bpp_sign_pub[i]} epub=${onix_bpp_encr_pub[i]}
    fi
    echo "  $role"
    echo "    subscriber_id       : $id"
    echo "    subscriber_url      : $url"
    echo "    type                : $role"
    [[ -n "${onix_net_domain[i]}" ]] && echo "    domain              : ${onix_net_domain[i]}"
    echo "    unique_key_id       : $key_id"
    echo "    signing_public_key  : $spub"
    echo "    encr_public_key     : $epub"
    if [[ "${onix_net_registry_type[i]}" == "legacy" ]]; then
        echo "    valid_from          : $onix_valid_from"
        echo "    valid_until         : $onix_valid_until"
    fi
}

# onix_emit_registration_info - what the operator must register with each
# registry. Onboarding with an external registry is out-of-band.
onix_emit_registration_info() {
    local i
    onix_valid_dates
    echo "Registry registration details for this ONIX node"
    echo "Generated on $(date -u +"%Y-%m-%dT%H:%M:%SZ"). Only public keys are listed here."
    for ((i = 0; i < onix_net_count; i++)); do
        echo ""
        if [[ "${onix_net_registry_type[i]}" == "dedi" ]]; then
            echo "Network ${onix_net_name[i]:-$((i + 1))}: Beckn One / DeDi registry ($ONIX_DEDI_REGISTRY_URL)${onix_net_network_ids[i]:+, networks: ${onix_net_network_ids[i]}}"
            echo "  Register each subscriber below with the network's DeDi registry (Beckn One) and make sure"
            echo "  the record ID matches unique_key_id (update keyId in adapter.yaml if the registry assigns a different one)."
        else
            echo "Network ${onix_net_name[i]:-$((i + 1))}: registry ${onix_net_registry_url[i]}"
            echo "  Register each subscriber below with the registry (e.g. POST ${onix_net_registry_url[i]}/register) and"
            echo "  ask the registry administrator to approve / subscribe it."
        fi
        if onix_has_bap; then onix_emit_participant_info "$i" BAP; fi
        if onix_has_bpp; then onix_emit_participant_info "$i" BPP; fi
    done
}

# onix_emit_compose_env - environment for docker-compose-onix-node.yml.
onix_emit_compose_env() {
    cat <<EOF
# Generated by install/beckn-onix.sh. Used as:
#   docker compose --env-file <this file> -f install/docker-compose-onix-node.yml up -d
ONIX_IMAGE=$onix_image
ONIX_HOST_PORT=$onix_host_port
ONIX_BIND_ADDRESS=$onix_bind_address
EOF
    # The "both" profile starts the BPP modules' own Redis.
    if [[ "$onix_role" == "BOTH" ]]; then
        echo "COMPOSE_PROFILES=both"
    fi
}

# onix_confirm_config_replacement - ask, before any input is collected,
# whether an existing ONIX node configuration may be replaced (it is backed up
# when the new one is written). Fails if the user declines.
onix_confirm_config_replacement() {
    onix_replace_confirmed=0
    [[ -e "$ONIX_NODE_CONFIG_DIR" ]] || return 0
    echo "${YELLOW}An ONIX node configuration already exists at $ONIX_NODE_CONFIG_DIR.${NC}"
    if onix_prompt_yes_no "Replace it? (it is backed up first)" N; then
        onix_replace_confirmed=1
        return 0
    fi
    echo "Keeping the existing configuration; nothing was changed."
    return 1
}

# onix_prepare_config_dir - create ONIX_NODE_CONFIG_DIR, moving an existing
# configuration to a timestamped backup once replacement is confirmed.
onix_prepare_config_dir() {
    local backup
    if [[ -e "$ONIX_NODE_CONFIG_DIR" ]]; then
        if [[ "${onix_replace_confirmed:-0}" != "1" ]] && ! onix_confirm_config_replacement; then
            return 1
        fi
        backup="$ONIX_NODE_CONFIG_DIR.backup-$(date +%Y%m%d%H%M%S)"
        mv "$ONIX_NODE_CONFIG_DIR" "$backup" || return 1
        echo "${GREEN}✓ Existing configuration moved to $backup${NC}"
    fi
    mkdir -p "$ONIX_NODE_CONFIG_DIR"
}

# onix_write_config - write adapter.yaml, routing files, registration info and
# the compose environment into ONIX_NODE_CONFIG_DIR.
onix_write_config() {
    local i role kind lrole name
    onix_prepare_config_dir || return 1

    (umask 077 && onix_emit_adapter_config >"$ONIX_NODE_CONFIG_DIR/adapter.yaml") || return 1
    for ((i = 0; i < onix_net_count; i++)); do
        for role in BAP BPP; do
            if [[ "$role" == "BAP" ]] && ! onix_has_bap; then continue; fi
            if [[ "$role" == "BPP" ]] && ! onix_has_bpp; then continue; fi
            lrole=$(printf '%s' "$role" | tr '[:upper:]' '[:lower:]')
            for kind in Receiver Caller; do
                name=$(onix_module_name "$i" "${lrole}Txn$kind")
                onix_emit_routing "$i" "$role" "$kind" >"$ONIX_NODE_CONFIG_DIR/routing-$name.yaml" || return 1
            done
        done
    done
    onix_emit_registration_info >"$ONIX_NODE_CONFIG_DIR/registration-info.txt" || return 1
    onix_emit_compose_env >"$ONIX_NODE_CONFIG_DIR/$ONIX_NODE_ENV_FILE_NAME" || return 1
    echo "${GREEN}✓ ONIX node configuration written to $ONIX_NODE_CONFIG_DIR${NC}"
}

# ---------------------------------------------------------------------------
# Legacy registry registration (optional)
# ---------------------------------------------------------------------------

# onix_legacy_registry_login REGISTRY_URL USER PASSWORD - prints the API key.
# Login lives next to the subscriber API: <base>/login for <base>/subscribers.
onix_legacy_registry_login() {
    local login_url="${1%/subscribers}/login" body response api_key
    body=$(jq -n --arg n "$2" --arg p "$3" '{Name: $n, Password: $p}') || return 1
    response=$(curl -s -H "Accept: application/json" -H "Content-Type: application/json" -d "$body" "$login_url") || return 1
    api_key=$(printf '%s' "$response" | jq -r '.api_key // empty' 2>/dev/null)
    if [[ -z "$api_key" ]]; then
        # Some registry versions expect the credentials wrapped in a User object.
        body=$(jq -n --arg n "$2" --arg p "$3" '{User: {Name: $n, Password: $p}}') || return 1
        response=$(curl -s -H "Accept: application/json" -H "Content-Type: application/json" -d "$body" "$login_url") || return 1
        api_key=$(printf '%s' "$response" | jq -r '.api_key // empty' 2>/dev/null)
    fi
    [[ -n "$api_key" ]] || return 1
    printf '%s' "$api_key"
}

# onix_legacy_register INDEX ROLE API_KEY - POST <registry>/register.
onix_legacy_register() {
    local i=$1 role=$2 api_key=$3 id url key_id spub epub body status
    if [[ "$role" == "BAP" ]]; then
        id=${onix_bap_id[i]} url=${onix_bap_url[i]} key_id=${onix_bap_key_id[i]} spub=${onix_bap_sign_pub[i]} epub=${onix_bap_encr_pub[i]}
    else
        id=${onix_bpp_id[i]} url=${onix_bpp_url[i]} key_id=${onix_bpp_key_id[i]} spub=${onix_bpp_sign_pub[i]} epub=${onix_bpp_encr_pub[i]}
    fi
    onix_valid_dates
    body=$(jq -n --arg id "$id" --arg key "$key_id" --arg url "$url" --arg domain "${onix_net_domain[i]}" \
        --arg spub "$spub" --arg epub "$epub" --arg from "$onix_valid_from" --arg until "$onix_valid_until" --arg type "$role" \
        '{subscriber_id: $id, pub_key_id: $key, unique_key_id: $key, subscriber_url: $url, domain: $domain,
          extended_attributes: {domains: []}, encr_public_key: $epub, signing_public_key: $spub,
          valid_from: $from, valid_until: $until, type: $type, country: "IND", status: "SUBSCRIBED"}') || return 1
    status=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${onix_net_registry_url[i]}/register" \
        -H "ApiKey:$api_key" -H "Content-Type: application/json" --data-raw "$body")
    if [[ "$status" =~ ^2 ]]; then
        echo "${GREEN}✓ Registered $role $id with ${onix_net_registry_url[i]}${NC}"
        return 0
    fi
    echo "${RED}Error: registering $role $id with ${onix_net_registry_url[i]} failed (HTTP $status). Register it manually.${NC}"
    return 1
}

# onix_offer_legacy_registration - for networks with a /lookup registry, offer
# to create the subscriber entries with the operator's registry credentials.
onix_offer_legacy_registration() {
    local i user password api_key
    for ((i = 0; i < onix_net_count; i++)); do
        [[ "${onix_net_registry_type[i]}" == "legacy" ]] || continue
        echo ""
        if ! onix_prompt_yes_no "Create the subscriber entries in ${onix_net_registry_url[i]} now with your registry credentials?" N; then
            continue
        fi
        onix_prompt user "Registry username"
        onix_read password "Registry password: " secret
        if ! api_key=$(onix_legacy_registry_login "${onix_net_registry_url[i]}" "$user" "$password"); then
            echo "${RED}Login to the registry failed. Register the subscribers manually using registration-info.txt.${NC}"
            continue
        fi
        if onix_has_bap; then onix_legacy_register "$i" BAP "$api_key"; fi
        if onix_has_bpp; then onix_legacy_register "$i" BPP "$api_key"; fi
    done
    return 0
}

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------

onix_compose() {
    COMPOSE_IGNORE_ORPHANS=1 docker compose --env-file "$ONIX_NODE_CONFIG_DIR/$ONIX_NODE_ENV_FILE_NAME" -f "$ONIX_NODE_COMPOSE_FILE" "$@"
}

# onix_start_node - start Redis and the adapter and wait for /health.
onix_start_node() {
    local attempt health_host=127.0.0.1 running
    echo "${GREEN}................Starting Redis and ONIX adapter................${NC}"
    # The adapter reads adapter.yaml only at start-up, and "up -d" leaves a
    # running container alone when only its mounted configuration changed, so
    # restart an adapter that was already running (re-run of the installer).
    running=$(docker ps -q --filter "name=^onix-node$" 2>/dev/null)
    if ! onix_compose up -d || { [[ -n "$running" ]] && ! onix_compose restart onix-node; }; then
        echo "${RED}Error: failed to start the ONIX node containers.${NC}"
        return 1
    fi
    for ((attempt = 1; attempt <= 15; attempt++)); do
        if curl -sf "http://$health_host:$onix_host_port/health" >/dev/null 2>&1; then
            echo "${GREEN}✓ ONIX adapter is healthy on port $onix_host_port${NC}"
            return 0
        fi
        sleep 2
    done
    echo "${YELLOW}The adapter did not report healthy yet. Check its logs:${NC}"
    echo "  docker logs onix-node"
    return 1
}

# onix_print_next_steps - endpoints, proxy/TLS guidance and management commands.
onix_print_next_steps() {
    local i lrole env_file="$ONIX_NODE_CONFIG_DIR/$ONIX_NODE_ENV_FILE_NAME"
    echo ""
    echo "${BLUE}Expose ONLY these paths publicly (reverse proxy with TLS -> http://<this host>:$onix_host_port):${NC}"
    for ((i = 0; i < onix_net_count; i++)); do
        for lrole in bap bpp; do
            if [[ "$lrole" == "bap" ]] && ! onix_has_bap; then continue; fi
            if [[ "$lrole" == "bpp" ]] && ! onix_has_bpp; then continue; fi
            echo "  ${onix_net_prefix[i]}/$lrole/receiver/"
        done
    done
    echo "${BLUE}Your application calls these private endpoints (http://onix-node:$ONIX_NODE_HTTP_PORT on the beckn_network Docker network, or http://127.0.0.1:$onix_host_port):${NC}"
    for ((i = 0; i < onix_net_count; i++)); do
        for lrole in bap bpp; do
            if [[ "$lrole" == "bap" ]] && ! onix_has_bap; then continue; fi
            if [[ "$lrole" == "bpp" ]] && ! onix_has_bpp; then continue; fi
            echo "  ${onix_net_prefix[i]}/$lrole/caller/<action>"
        done
    done
    echo ""
    echo "Configuration : $ONIX_NODE_CONFIG_DIR/adapter.yaml (contains private keys)"
    echo "Registration  : $ONIX_NODE_CONFIG_DIR/registration-info.txt"
    echo "Restart       : docker compose --env-file $env_file -f $ONIX_NODE_COMPOSE_FILE up -d --force-recreate onix-node"
    echo "Logs          : docker logs -f onix-node"
    echo "Stop          : docker compose --env-file $env_file -f $ONIX_NODE_COMPOSE_FILE down"
}
