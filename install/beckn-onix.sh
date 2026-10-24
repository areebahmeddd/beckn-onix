#!/bin/bash
source scripts/variables.sh
source scripts/get_container_details.sh
source scripts/onix_node.sh

# Function to start a specific service inside docker-compose file
install_package() {
    echo "${GREEN}................Installing required packages................${NC}"
    bash scripts/package_manager.sh
    echo "Package Installation is done"

}
start_container() {
    #ignore orphaned containers warning
    export COMPOSE_IGNORE_ORPHANS=1
    docker compose -f $1 up -d $2
}

update_registry_details() {
    if [[ $1 ]]; then
        if [[ $1 == https://* ]]; then
            if [[ $(uname -s) == 'Darwin' ]]; then
                registry_url=$(echo "$1" | sed -E 's/https:\/\///')
            else
                registry_url=$(echo "$1" | sed 's/https:\/\///')
            fi
            registry_port=443
            protocol=https
        elif [[ $1 == http://* ]]; then
            if [[ $(uname -s) == 'Darwin' ]]; then
                registry_url=$(echo "$1" | sed -E 's/http:\/\///')
            else
                registry_url=$(echo "$1" | sed 's/http:\/\///')
            fi
            registry_port=80
            protocol=http
        fi

    else
        registry_url=registry
        registry_port=3030
        protocol=http
    fi
    echo $registry_url
    cp $SCRIPT_DIR/../registry_data/config/swf.properties-sample $SCRIPT_DIR/../registry_data/config/swf.properties
    config_file="$SCRIPT_DIR/../registry_data/config/swf.properties"

    tmp_file=$(mktemp "tempfile.XXXXXXXXXX")
    sed "s|REGISTRY_URL|$registry_url|g; s|REGISTRY_PORT|$registry_port|g; s|PROTOCOL|$protocol|g" "$config_file" >"$tmp_file"
    mv "$tmp_file" "$config_file"
    docker volume create registry_data_volume
    docker volume create registry_database_volume
    docker run --rm -v $SCRIPT_DIR/../registry_data/config:/source -v registry_data_volume:/target busybox cp /source/{envvars,logger.properties,swf.properties} /target/
    docker rmi busybox
}
# Function to start Redis service only
start_support_services() {
    #ignore orphaned containers warning
    export COMPOSE_IGNORE_ORPHANS=1
    echo "${GREEN}................Installing Redis................${NC}"
    docker compose -f docker-compose-app.yml up -d redis_db
    echo "Redis installation successful"
}

install_gateway() {
    if [[ $1 && $2 ]]; then
        bash scripts/update_gateway_details.sh $1 $2
    else
        bash scripts/update_gateway_details.sh http://registry:3030
    fi
    echo "${GREEN}................Installing Gateway service................${NC}"
    start_container $gateway_docker_compose_file gateway
    echo "Registering Gateway in the registry"

    sleep 10
    # if [[ $1 && $2 ]]; then
    #     bash scripts/register_gateway.sh $2
    # else
    #     bash scripts/register_gateway.sh
    # fi
    echo " "
    echo "Gateway installation successful"
}

# Function to install Beckn Registry
# Usage: install_registry [public_registry_url] [admin_registry_url]
# Role permissions are uploaded via admin_registry_url (default
# public_registry_url), as a new production registry is not public yet.
install_registry() {
    if [[ $1 ]]; then
        update_registry_details $1
    else
        update_registry_details
    fi

    echo "${GREEN}................Installing Registry service................${NC}"
    start_container $registry_docker_compose_file registry
    sleep 10
    echo "Registry installation successful"

    #Update Role Permission for registry.
    if [[ $2 ]]; then
        bash scripts/registry_role_permissions.sh "$2"
    elif [[ $1 ]]; then
        bash scripts/registry_role_permissions.sh $1
    else
        bash scripts/registry_role_permissions.sh
    fi
}

# Function to install Layer2 Config
install_layer2_config() {
    container_name=$1
    FILENAME="$(basename "$layer2_url")"
    wget -O "$(basename "$layer2_url")" "$layer2_url" >/dev/null 2>&1
    if [ $? -eq 0 ]; then
        docker cp "$FILENAME" $container_name:"$schemas_path/$FILENAME" >/dev/null 2>&1
        if [ $? -eq 0 ]; then
            echo "${GREEN}Successfully copied $FILENAME to Docker container $container_name.${NC}"
        fi
    else
        echo "${BoldRed}The Layer 2 configuration file has not been downloaded.${NC}"
        echo -e "${BoldGreen}Please download the Layer 2 configuration files by running the download_layer_2_config_bap.sh script located in the ../layer2 folder."
        echo -e "For further information, refer to this URL: https://github.com/beckn/beckn-onix/blob/main/docs/user_guide.md#downloading-layer-2-configuration-for-a-domain.${NC}"
    fi
    rm -f $FILENAME >/dev/null 2>&1
}

# Function to install BAP Protocol Server - creates registry entries only
install_bap_protocol_server() {
    bash scripts/update_bap_config.sh
    echo "Protocol server BAP registry entries created successfully"
}

# Function to install BPP Protocol Server with Sandbox
install_bpp_protocol_server_with_sandbox() {
    echo "${GREEN}................Installing Sandbox................${NC}"
    start_container $bpp_docker_compose_file_sandbox "sandbox-api"
    sleep 5
    echo "Sandbox installation successful"

    echo "${GREEN}................Installing Protocol Server for BPP................${NC}"
    bash scripts/update_bpp_config.sh
    echo "Protocol server BPP registry entries created successfully"
}

layer2_config() {
    while true; do
        read -p "Paste the URL of the Layer 2 configuration here (or press Enter to skip): " layer2_url
        if [[ -z "$layer2_url" ]]; then
            break #If URL is empty then skip the URL validation
        elif [[ $layer2_url =~ ^(http|https):// ]]; then
            break
        else
            echo "${RED}Invalid URL format. Please enter a valid URL starting with http:// or https://.${NC}"
        fi
    done
}

restart_script() {
    read -p "${GREEN}Do you want to restart the script or exit the script? (r for restart, e for exit): ${NC}" choice
    if [[ $choice == "r" ]]; then
        echo "Restarting the script..."
        exec "$0" # Restart the script by re-executing it
    elif [[ $choice == "e" ]]; then
        echo "Exiting the script..."
        exit 0
    fi
}

# Function to validate user input
validate_input() {
    local input=$1
    local max_option=$2

    # Check if the input is a digit and within the valid range
    if [[ "$input" =~ ^[0-9]+$ ]] && ((input >= 1 && input <= max_option)); then
        return 0 # Valid input
    else
        echo "${RED}Invalid input. Please enter a number between 1 and $max_option.${NC}"
        return 1 # Invalid input
    fi
}

check_docker_permissions() {
    if ! command -v docker &>/dev/null; then
        echo -e "${RED}Error: Docker is not installed on this system.${NC}"
        if [[ "$OSTYPE" == "linux-gnu"* ]]; then
            install_package
            if [[ $? -ne 0 ]]; then
                echo -e "${RED}Please install Docker and try again.${NC}"
                echo -e "${RED}Please install Docker and jq manually.${NC}"
                exit 1
            fi
        fi
    fi
    if [[ "$OSTYPE" == "linux-gnu"* ]]; then
        if ! groups "$USER" | grep -q '\bdocker\b'; then
            echo -e "${RED}Error: You do not have permission to run Docker. Please add yourself to the docker group by running the following command:${NC}"
            echo -e "${BoldGreen}sudo usermod -aG docker \$USER"
            echo -e "After running the above command, please log out and log back in to your system, then restart the deployment script.${NC}"
            exit 1
        fi
    fi
}

# Function to update/upgrade a specific service
update_service() {
    service_name=$1
    docker_compose_file=$2
    image_name=$3

    echo "${GREEN}................Updating $service_name................${NC}"

    export COMPOSE_IGNORE_ORPHANS=1
    # Pull the latest image
    docker pull "$image_name"

    # Stop and remove the existing container
    docker compose -f "$docker_compose_file" stop "$service_name"
    docker compose -f "$docker_compose_file" rm -f "$service_name"

    # Start the service with the new image
    docker compose -f "$docker_compose_file" up -d "$service_name"

    echo "$service_name update successful"
}

# Function to validate required modules exist in config file
validate_config_modules() {
    local key_type=$1
    local config_file=$2
    
    local has_bap_modules=$(grep -c "name: bapTxn" "$config_file")
    local has_bpp_modules=$(grep -c "name: bppTxn" "$config_file")
    
    # Only validate that required modules exist, don't enforce strict matching
    if [[ "$key_type" == "BAP" && $has_bap_modules -eq 0 ]]; then
        echo "${RED}Error: BAP deployment selected but no BAP modules found in config file${NC}"
        echo "${BLUE}Config file: $config_file${NC}"
        echo "${YELLOW}Please update docker-compose-adapter.yml to use a config file with BAP modules${NC}"
        return 1
    elif [[ "$key_type" == "BPP" && $has_bpp_modules -eq 0 ]]; then
        echo "${RED}Error: BPP deployment selected but no BPP modules found in config file${NC}"
        echo "${BLUE}Config file: $config_file${NC}"
        echo "${YELLOW}Please update docker-compose-adapter.yml to use a config file with BPP modules${NC}"
        return 1
    elif [[ "$key_type" == "BOTH" && ($has_bap_modules -eq 0 || $has_bpp_modules -eq 0) ]]; then
        echo "${RED}Error: Combined deployment selected but missing modules in config file${NC}"
        echo "${BLUE}Config file: $config_file${NC}"
        echo "${BLUE}BAP modules found: $has_bap_modules, BPP modules found: $has_bpp_modules${NC}"
        echo "${YELLOW}Please update docker-compose-adapter.yml to use a config file with both BAP and BPP modules${NC}"
        return 1
    fi
    
    # Allow BAP deployment with combined config (BAP + BPP modules)
    # Allow BPP deployment with combined config (BAP + BPP modules)
    echo "${GREEN}✓ Config validation passed - $key_type deployment with available modules${NC}"
    return 0
}

# Function to configure ONIX adapter with registry keys
# Usage: configure_onix_registry_keys [BAP|BPP|BOTH] [config_file]
configure_onix_registry_keys() {
    local key_type=${1:-"BOTH"}
    local config_file=$2
    
    echo "${GREEN}Setting up ONIX keys ($key_type) from existing protocol server configs...${NC}"
    
    # Determine config file - only auto-detect if not provided
    if [ -z "$config_file" ]; then
        local docker_config=$(grep -o '/app/config/[^"]*' docker-compose-adapter.yml | head -1)
        if [ -z "$docker_config" ]; then
            echo "${RED}Error: Could not find config file in docker-compose-adapter.yml${NC}"
            return 1
        fi
        config_file="${docker_config/\/app\/config\//../config/}"
        echo "${BLUE}detected config file from docker-compose: $config_file${NC}"
    else
        echo "${BLUE}Using specified config file: $config_file${NC}"
    fi
    
    echo "${BLUE}Updating config file: $config_file${NC}"
    
    local bap_config="protocol-server-data/bap-client.yml"
    local bpp_config="protocol-server-data/bpp-client.yml"
    
    # Extract keys based on type
    local bap_private_key bap_public_key bpp_private_key bpp_public_key
    
    if [[ "$key_type" == "BAP" || "$key_type" == "BOTH" ]]; then
        if [ -f "$bap_config" ]; then
            bap_private_key_full=$(awk '/privateKey:/ {print $2}' "$bap_config" | tr -d '"')
            bap_public_key=$(awk '/publicKey:/ {print $2}' "$bap_config" | tr -d '"')
            bap_private_key=$(echo "$bap_private_key_full" | base64 -d | head -c 32 | base64 -w 0)
            echo "${GREEN}✓ Extracted BAP keys from $bap_config${NC}"
        else
            echo "${RED}Error: BAP config file not found at $bap_config${NC}"
            return 1
        fi
    fi
    
    if [[ "$key_type" == "BPP" || "$key_type" == "BOTH" ]]; then
        if [ -f "$bpp_config" ]; then
            bpp_private_key_full=$(awk '/privateKey:/ {print $2}' "$bpp_config" | tr -d '"')
            bpp_public_key=$(awk '/publicKey:/ {print $2}' "$bpp_config" | tr -d '"')
            bpp_private_key=$(echo "$bpp_private_key_full" | base64 -d | head -c 32 | base64 -w 0)
            echo "${GREEN}✓ Extracted BPP keys from $bpp_config${NC}"
        else
            echo "${YELLOW}⚠ BPP config file not found at $bpp_config - BPP modules will be skipped${NC}"
            # Don't return error, just skip BPP key extraction
        fi
    fi
    
    if [ ! -f "$config_file" ]; then
        echo "${RED}Error: ONIX config file not found at $config_file${NC}"
        return 1
    fi
    
    # Validate required modules exist
    validate_config_modules "$key_type" "$config_file"
    if [ $? -ne 0 ]; then
        return 1
    fi
    
    # Detect indentation pattern from the file
    local steps_indent=$(grep -m1 "steps:" "$config_file" | sed 's/steps:.*//' | wc -c)
    steps_indent=$((steps_indent - 1))  # Remove newline
    local plugin_indent=$((steps_indent + 2))
    local config_indent=$((steps_indent + 4))
    
    # Process the config file with dynamic indentation
    awk -v bap_pub="$bap_public_key" -v bap_priv="$bap_private_key" \
        -v bpp_pub="$bpp_public_key" -v bpp_priv="$bpp_private_key" \
        -v key_type="$key_type" \
        -v steps_spaces="$steps_indent" -v plugin_spaces="$plugin_indent" -v config_spaces="$config_indent" '
    BEGIN { 
        module_type = ""
        in_keymanager = 0
        has_keymanager = 0
        # Create indent strings
        steps_indent = sprintf("%*s", steps_spaces, "")
        plugin_indent = sprintf("%*s", plugin_spaces, "")
        config_indent = sprintf("%*s", config_spaces, "")
        steps_pattern = "^" steps_indent "steps:"
    }
    
    # Track module type and reset keymanager flag
    /- name: bapTxn/ { 
        if ((key_type == "BAP" || key_type == "BOTH") && bap_priv != "") module_type = "bap"
        else module_type = ""
        has_keymanager = 0; print; next 
    }
    /- name: bppTxn/ { 
        if ((key_type == "BPP" || key_type == "BOTH") && bpp_priv != "") module_type = "bpp"
        else module_type = ""
        has_keymanager = 0; print; next 
    }
    /^  - name:/ && !/bapTxn/ && !/bppTxn/ { module_type = ""; has_keymanager = 0; print; next }
    
    # Handle keyManager section
    /keyManager:/ {
        in_keymanager = 1
        has_keymanager = 1
        print
        next
    }
    
    # Force simplekeymanager id
    in_keymanager && /id:/ {
        print config_indent "id: simplekeymanager"
        next
    }
    
    # Replace config section entirely
    in_keymanager && /config:/ {
        print config_indent "config:"
        # Skip existing config lines until next plugin or steps
        while (getline) {
            if (match($0, "^" plugin_indent "[a-zA-Z]") || match($0, steps_pattern)) {
                # Add complete config based on module
                if (module_type == "bap") {
                    print config_indent "  networkParticipant: bap-network"
                    print config_indent "  keyId: bap-network-key"
                    print config_indent "  signingPrivateKey: \"" bap_priv "\""
                    print config_indent "  signingPublicKey: \"" bap_pub "\""
                    print config_indent "  encrPrivateKey: \"" bap_priv "\""
                    print config_indent "  encrPublicKey: \"" bap_pub "\""
                } else if (module_type == "bpp") {
                    print config_indent "  networkParticipant: bpp-network"
                    print config_indent "  keyId: bpp-network-key"
                    print config_indent "  signingPrivateKey: \"" bpp_priv "\""
                    print config_indent "  signingPublicKey: \"" bpp_pub "\""
                    print config_indent "  encrPrivateKey: \"" bpp_priv "\""
                    print config_indent "  encrPublicKey: \"" bpp_pub "\""
                }
                in_keymanager = 0
                print
                next
            }
        }
        next
    }
    
    # Exit keyManager on unindented line
    in_keymanager && match($0, "^" plugin_indent "[a-zA-Z]") && !match($0, "^" config_indent) {
        in_keymanager = 0
    }
    
    # Add keyManager if missing before steps
    module_type && match($0, steps_pattern) && !has_keymanager {
        print plugin_indent "keyManager:"
        print config_indent "id: simplekeymanager"
        print config_indent "config:"
        if (module_type == "bap") {
            print config_indent "  networkParticipant: bap-network"
            print config_indent "  keyId: bap-network-key"
            print config_indent "  signingPrivateKey: \"" bap_priv "\""
            print config_indent "  signingPublicKey: \"" bap_pub "\""
            print config_indent "  encrPrivateKey: \"" bap_priv "\""
            print config_indent "  encrPublicKey: \"" bap_pub "\""
        } else if (module_type == "bpp") {
            print config_indent "  networkParticipant: bpp-network"
            print config_indent "  keyId: bpp-network-key"
            print config_indent "  signingPrivateKey: \"" bpp_priv "\""
            print config_indent "  signingPublicKey: \"" bpp_pub "\""
            print config_indent "  encrPrivateKey: \"" bpp_priv "\""
            print config_indent "  encrPublicKey: \"" bpp_pub "\""
        }
    }
    
    # Print all other lines
    { print }
    ' "$config_file" > "${config_file}.tmp"
    
    if [ $? -eq 0 ] && [ -f "${config_file}.tmp" ]; then
        mv "${config_file}.tmp" "$config_file"
        echo "${GREEN}✓ ONIX keys configured with existing registry keys${NC}"
    else
        echo "${RED}Error: AWK script failed to process config file${NC}"
        rm -f "${config_file}.tmp"
        return 1
    fi
    [[ "$key_type" == "BAP" || "$key_type" == "BOTH" ]] && echo "${GREEN}✓ BAP modules using BAP keys${NC}"
    [[ "$key_type" == "BPP" || "$key_type" == "BOTH" ]] && echo "${GREEN}✓ BPP modules using BPP keys${NC}"
    
    return 0
}

# Function to handle the update/upgrade process
update_network() {
    echo -e "\nWhich component would you like to update?\n1. Registry\n2. Gateway\n3. BAP Protocol Server\n4. BPP Protocol Server\n5. All components"
    read -p "Enter your choice: " update_choice

    validate_input "$update_choice" 5
    if [[ $? -ne 0 ]]; then
        restart_script
    fi

    case $update_choice in
    1)
        update_service "registry" "$registry_docker_compose_file" "fidedocker/registry"
        ;;
    2)
        update_service "gateway" "$gateway_docker_compose_file" "fidedocker/gateway"
        ;;
    3)
        update_service "bap-client" "$bap_docker_compose_file" "fidedocker/protocol-server"
        update_service "bap-network" "$bap_docker_compose_file" "fidedocker/protocol-server"
        ;;
    4)
        update_service "bpp-client" "$bpp_docker_compose_file" "fidedocker/protocol-server"
        update_service "bpp-network" "$bpp_docker_compose_file" "fidedocker/protocol-server"
        ;;
    5)
        update_service "registry" "$registry_docker_compose_file" "fidedocker/registry"
        update_service "gateway" "$gateway_docker_compose_file" "fidedocker/gateway"
        update_service "bap-client" "$bap_docker_compose_file" "fidedocker/protocol-server"
        update_service "bap-network" "$bap_docker_compose_file" "fidedocker/protocol-server"
        update_service "bpp-client" "$bpp_docker_compose_file" "fidedocker/protocol-server"
        update_service "bpp-network" "$bpp_docker_compose_file" "fidedocker/protocol-server"
        ;;
    *)
        echo "Unknown choice"
        ;;
    esac
}

# Function to install ONIX adapter with specified key type
install_adapter() {
    local key_type=${1:-"BOTH"}
    local config_file=$2
    
    # Create schemas directory for validation
    if [ ! -d "schemas" ]; then
        mkdir -p schemas
        echo -e "${GREEN}✓ Created schemas directory${NC}"
    else
        echo -e "${YELLOW}schemas directory already exists${NC}"
    fi

    echo "${GREEN}................Building plugins for ONIX Adapter................${NC}"
    
    # Build plugins the same way as setup.sh
    cd ..
    if [ -f "./install/build-plugins.sh" ]; then
        chmod +x ./install/build-plugins.sh
        ./install/build-plugins.sh
        if [ $? -eq 0 ]; then
            echo "${GREEN}✓ Plugins built successfully${NC}"
        else
            echo "${RED}Error: Plugin build failed${NC}"
            exit 1
        fi
    else
        echo "${RED}Error: install/build-plugins.sh not found${NC}"
        exit 1
    fi
    cd install
    
    echo "${GREEN}................Setting up keys for ONIX Adapter ($key_type)................${NC}"
    configure_onix_registry_keys "$key_type" "$config_file"
    if [ $? -ne 0 ]; then
        echo "${RED}ONIX Adapter installation failed due to configuration errors${NC}"
        exit 1
    fi
    
    echo "${GREEN}................Starting Redis and ONIX Adapter................${NC}"
    start_support_services
    start_container $adapter_docker_compose_file "onix-adapter"
    sleep 10
    echo "ONIX Adapter installation successful"
}

# Helper function for BAP-only ONIX setup
install_bap_adapter() {
    install_adapter "BAP"
}

# Helper function for BPP-only ONIX setup  
install_bpp_adapter() {
    install_adapter "BPP"
}

# Function to install Beckn One Adapter (Option 3)
install_beckn_one_adapter() {
    echo "${GREEN}................Setting up network with Beckn One................${NC}"
    
    # Create schemas directory if not exists
    if [ ! -d "schemas" ]; then
        mkdir -p schemas
        echo -e "${GREEN}✓ Created schemas directory${NC}"
    fi

    echo "${GREEN}................Building plugins for ONIX Adapter................${NC}"
    
    # Build plugins
    cd ..
    if [ -f "./install/build-plugins.sh" ]; then
        chmod +x ./install/build-plugins.sh
        ./install/build-plugins.sh
        if [ $? -eq 0 ]; then
            echo "${GREEN}✓ Plugins built successfully${NC}"
        else
            echo "${RED}Error: Plugin build failed${NC}"
            exit 1
        fi
    else
        echo "${RED}Error: install/build-plugins.sh not found${NC}"
        exit 1
    fi
    cd install
    
    echo "${GREEN}................Starting Redis and Beckn One Adapters................${NC}"
    
    # Start Redis first
    start_support_services
    
    # Start Beckn One adapters (onix-bap, onix-bpp, sandbox-bap, sandbox-bpp)
    start_container $adapter_beckn_one_docker_compose_file "onix-bap"
    start_container $adapter_beckn_one_docker_compose_file "onix-bpp"
    start_container $adapter_beckn_one_docker_compose_file "sandbox-bap"
    start_container $adapter_beckn_one_docker_compose_file "sandbox-bpp"
    
    sleep 10
    
    echo "${GREEN}✓ Beckn One network setup complete${NC}"
    echo ""
    echo "${BLUE}Services running:${NC}"
    echo "  - ONIX BAP Adapter: http://localhost:8081"
    echo "  - ONIX BPP Adapter: http://localhost:8082"
    echo "  - Sandbox BAP: http://localhost:3001"
    echo "  - Sandbox BPP: http://localhost:3002"
    echo "  - Redis: localhost:6379"
}

# Function to set up the ONIX adapter node from the collected onix_* inputs
setup_onix_node() {
    onix_print_summary
    if ! onix_prompt_yes_no "Proceed with this setup?" Y; then
        restart_script
        return 1
    fi

    install_package
    deploy_onix_node
}

# Function to deploy the ONIX adapter node without asking for confirmation
# Usage: deploy_onix_node [local_registry_admin_url]
# With local_registry_admin_url the subscribers are registered in the registry
# this script installed; otherwise registration is offered.
deploy_onix_node() {
    local api_key

    echo "${GREEN}................Preparing keys for ONIX adapter................${NC}"
    if ! onix_resolve_all_keys; then
        echo "${RED}ONIX node setup aborted: keys could not be prepared${NC}"
        exit 1
    fi
    if ! onix_write_config; then
        echo "${RED}ONIX node setup aborted: configuration was not written${NC}"
        exit 1
    fi

    echo ""
    cat "$ONIX_NODE_CONFIG_DIR/registration-info.txt"
    if [[ $1 ]]; then
        echo "${GREEN}................Registering the adapter in the network registry................${NC}"
        if api_key=$(onix_legacy_registry_login "$1" "$USERNAME" "$PASSWORD"); then
            onix_register_network_participants 0 "$api_key" "$1"
        else
            echo "${RED}Could not log in to the registry at $1; register the subscribers above manually.${NC}"
        fi
    else
        onix_offer_legacy_registration
    fi

    # The adapter image bundles all plugins, so nothing is built on the host.
    onix_start_node
    onix_print_next_steps
}

# Function to choose the bind address of the registry and gateway ports. It is
# saved in install/.env, which docker compose reads for every compose file
# here, so restarts and option 7 updates keep it.
configure_network_services_bind_address() {
    local addr="127.0.0.1" env_file=".env"
    echo "The registry and gateway publish ports 3000/3030 and 4000/4030; in production they sit behind a TLS reverse proxy."
    if ! onix_prompt_yes_no "Publish the registry and gateway ports on 127.0.0.1 only (recommended with a reverse proxy on this host)?" Y; then
        addr="0.0.0.0"
        echo "${YELLOW}Make sure a firewall keeps ports 3000, 3030, 4000 and 4030 private.${NC}"
    fi
    if [ -f "$env_file" ] && grep -q '^BECKN_SERVICES_BIND_ADDRESS=' "$env_file"; then
        sed "s|^BECKN_SERVICES_BIND_ADDRESS=.*|BECKN_SERVICES_BIND_ADDRESS=$addr|" "$env_file" >"$env_file.tmp" && mv "$env_file.tmp" "$env_file"
    else
        echo "BECKN_SERVICES_BIND_ADDRESS=$addr" >>"$env_file"
    fi
}

# Function to remove existing containers, once confirmed, so they are recreated
# with the new configuration (data volumes are kept)
# Usage: confirm_service_reinstall name...
confirm_service_reinstall() {
    local name existing=""
    command -v docker >/dev/null 2>&1 || return 0
    for name in "$@"; do
        if docker ps -a --format '{{.Names}}' | grep -qx "$name"; then
            existing="$existing $name"
        fi
    done
    [[ -n "$existing" ]] || return 0
    echo "${YELLOW}Existing container(s)$existing will be recreated with the new configuration (data volumes are kept).${NC}"
    onix_prompt_yes_no "Continue?" N || return 1
    for name in $existing; do
        docker rm -f "$name" >/dev/null
    done
    return 0
}

# Function to install a Beckn Gateway for a registry and optionally subscribe it.
# Usage: install_network_gateway registry_url gateway_url
install_network_gateway() {
    install_gateway "$1" "$2"
    echo ""
    echo "Subscribing the gateway needs the registry at $1 to be reachable from this machine."
    if onix_prompt_yes_no "Subscribe the gateway with the registry now?" N; then
        bash scripts/register_gateway.sh
        echo ""
    else
        echo "Subscribe it later with: cd install && bash scripts/register_gateway.sh"
    fi
}

# Function to join an existing network as a Beckn Gateway (legacy gateway service)
join_network_as_gateway() {
    local net_registry_url net_gateway_url
    echo "${GREEN}................Installing a Beckn Gateway for an existing network................${NC}"
    onix_prompt_url net_registry_url "Registry URL of the network (e.g. https://registry.example.com)"
    onix_prompt_url net_gateway_url "Public URL of this gateway (e.g. https://gateway.example.com)"
    confirm_service_reinstall gateway || { restart_script; return; }
    install_package
    configure_network_services_bind_address
    install_network_gateway "$net_registry_url" "$net_gateway_url"
    echo "Point your reverse proxy for $net_gateway_url at http://127.0.0.1:4030 and ask the registry operator to approve the gateway."
}

# Function to print what the operator must do after creating a production network.
print_production_network_next_steps() {
    local net_registry_url=$1 net_gateway_url=$2
    echo ""
    echo "${BLUE}Next steps for the production network${NC}"
    echo "1. Change the default password of '$USERNAME' on the registry${net_gateway_url:+ and the gateway} BEFORE they are"
    echo "   reachable from the internet; until then anyone who reaches them can administer the network."
    echo "   Log in from this machine (or an SSH tunnel) at http://127.0.0.1:3030/login${net_gateway_url:+ and http://127.0.0.1:4030/login}."
    echo "2. Point the public names at this machine and terminate TLS in a reverse proxy that forwards:"
    [[ -n "$net_registry_url" ]] && echo "     $net_registry_url -> http://127.0.0.1:3030"
    [[ -n "$net_gateway_url" ]] && echo "     $net_gateway_url -> http://127.0.0.1:4030"
    echo "   Ports 3000 and 4000 are not needed behind the proxy; keep them private."
    echo "3. Participants join with option 1, using $net_registry_url/subscribers as the registry URL."
}

# Function to create a new production network (Option 2)
create_production_network() {
    local choice components net_registry_url="" net_gateway_url="" with_gateway=0 with_adapter=0
    local adapter_search_url=""
    echo "${GREEN}................Creating a new production network................${NC}"
    echo "A network needs a registry; Beckn 1.x networks also use a gateway to broadcast search."
    echo -e "\nWhere will the network's registry live?"
    echo "1. Self-hosted Beckn registry (and gateway) on this machine"
    echo "2. Beckn One (hosted DeDi registry)"
    onix_prompt_choice choice "Enter your choice" 2
    if [[ $choice -eq 2 ]]; then
        echo ""
        echo "Beckn One networks are created, and participants approved, through Beckn One itself, so there is"
        echo "nothing to install (see https://docs.beckn.io/creating-an-open-network/setting-up-the-network-environment)."
        echo "Each participant then joins with an ONIX adapter and the network ID (option 1)."
        if onix_prompt_yes_no "Set up an ONIX adapter for that network on this machine now?" Y; then
            join_existing_network
        fi
        return
    fi

    echo -e "\nWhich components should run on this machine?"
    echo "1. Registry and gateway"
    echo "2. Registry only (no gateway, or a gateway hosted elsewhere)"
    echo "3. Gateway only (for an existing registry)"
    onix_prompt_choice components "Enter your choice" 3
    case $components in
    1) with_gateway=1 ;;
    2) ;;
    3)
        join_network_as_gateway
        return
        ;;
    esac

    onix_prompt_url net_registry_url "Public URL of the registry (e.g. https://registry.example.com)"
    if ((with_gateway)); then
        onix_prompt_url net_gateway_url "Public URL of the gateway (e.g. https://gateway.example.com)"
    fi
    if [[ "$net_registry_url" == http://* || "$net_gateway_url" == http://* ]]; then
        echo "${YELLOW}Warning: production registries and gateways should be served over HTTPS.${NC}"
    fi

    onix_reset_state
    echo ""
    if onix_prompt_yes_no "Also run an ONIX adapter on this machine as the network's first participant?" N; then
        with_adapter=1
        onix_collect_role
        if ! onix_confirm_config_replacement; then
            restart_script
            return
        fi
        # The adapter reaches the registry and gateway over the Docker network.
        onix_init_network 0 "" ""
        onix_use_legacy_registry 0 "http://registry:3030/subscribers" "$net_registry_url/subscribers"
        ((with_gateway)) && adapter_search_url="http://gateway:4030/bg"
        onix_collect_protocol 0 "$adapter_search_url"
        onix_collect_public_base_url
        if onix_has_bap; then onix_collect_participant 0 BAP; fi
        if onix_has_bpp; then onix_collect_participant 0 BPP; fi
        onix_collect_app_urls
        onix_collect_runtime
    fi

    echo -e "\n${BLUE}Production network summary${NC}"
    echo "  Registry : $net_registry_url (this machine, port 3030)"
    ((with_gateway)) && echo "  Gateway  : $net_gateway_url (this machine, port 4030)"
    ((with_adapter)) && onix_print_summary
    echo ""
    echo "The registry and gateway are configured for their public URLs, but setup reaches them on this machine."
    echo "The DNS and TLS proxy steps are printed at the end."
    if ! onix_prompt_yes_no "Proceed with this setup?" Y; then
        restart_script
        return
    fi
    # Ask about every existing container before removing any of them.
    if ((with_gateway)); then
        confirm_service_reinstall registry gateway || { restart_script; return; }
    else
        confirm_service_reinstall registry || { restart_script; return; }
    fi

    install_package
    configure_network_services_bind_address
    install_registry "$net_registry_url" "http://localhost:3030"
    if ((with_gateway)); then
        install_network_gateway "$net_registry_url" "$net_gateway_url"
    fi
    if ((with_adapter)); then
        deploy_onix_node "http://localhost:3030/subscribers"
    fi
    print_production_network_next_steps "$net_registry_url" "$net_gateway_url"
}

# Function to set up one ONIX adapter that participates in several networks
# (Option 5). Each network gets its own modules under /<name>/ with its own
# registry, subscriber IDs, keys and routing rules.
merge_networks() {
    local i=0 name
    echo "${GREEN}................Setting up one ONIX adapter for multiple networks................${NC}"
    echo "Each network gets its own modules under /<network name>/bap|bpp/receiver|caller/, with its"
    echo "own registry, subscriber IDs, keys and routing rules; one adapter and one port serve them all."
    echo "${BLUE}For several Beckn One networks that share a subscriber ID, option 1 with comma-separated${NC}"
    echo "${BLUE}network IDs is simpler: the DeDi registry plugin accepts all of them in one set of modules.${NC}"
    onix_reset_state
    onix_collect_role
    if ! onix_confirm_config_replacement; then
        restart_script
        return
    fi
    onix_collect_public_base_url

    while true; do
        echo -e "\n${GREEN}Network $((i + 1))${NC}"
        onix_collect_network_name name "$i"
        onix_collect_network "$i" "$name" "/$name"
        if onix_has_bap; then onix_collect_participant "$i" BAP; fi
        if onix_has_bpp; then onix_collect_participant "$i" BPP; fi
        i=$((i + 1))
        if ((i < 2)); then
            continue
        fi
        onix_prompt_yes_no "Add another network?" N || break
    done

    onix_collect_app_urls
    onix_collect_runtime
    setup_onix_node
}

# Function to join an existing network with an ONIX adapter (Option 1)
join_existing_network() {
    echo "${GREEN}................Joining an existing network with an ONIX adapter................${NC}"
    echo "Registering the node with the network's registry happens outside this script; the details are printed at the end."
    onix_reset_state
    onix_collect_role with_gateway
    if onix_role_is GATEWAY; then
        join_network_as_gateway
        return
    fi
    if ! onix_confirm_config_replacement; then
        restart_script
        return
    fi

    onix_collect_network 0 "" ""
    onix_collect_public_base_url
    if onix_has_bap; then onix_collect_participant 0 BAP; fi
    if onix_has_bpp; then onix_collect_participant 0 BPP; fi
    onix_collect_app_urls
    onix_collect_runtime
    setup_onix_node
}

# When sourced (e.g. by tests), only define the functions.
if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
    return 0
fi

# MAIN SCRIPT STARTS HERE

echo "Welcome to Beckn-ONIX!"
if [ -f ./onix_ascii_art.txt ]; then
    cat ./onix_ascii_art.txt
else
    echo "[Display Beckn-ONIX ASCII Art]"
fi

echo "Checking prerequisites of Beckn-ONIX deployment"
check_docker_permissions

echo "Beckn-ONIX is a platform that helps you quickly launch and configure beckn-enabled networks."
echo -e "\nWhat would you like to do?\n1. Join an existing network\n2. Create new production network\n3. Set up a network on local machine with Beckn One (DeDI-Registry, Catalog Discovery)\n4. Set up a network on local machine with local registry and gateway (without Beckn One)\n5. Merge multiple networks\n6. Configure Existing Network\n7. Update/Upgrade Application\n(Press Ctrl+C to exit)"
read -p "Enter your choice: " choice

validate_input "$choice" 7
if [[ $? -ne 0 ]]; then
    restart_script # Restart the script if input is invalid
fi

if [[ $choice -eq 1 ]]; then
    join_existing_network
elif [[ $choice -eq 2 ]]; then
    create_production_network
elif [[ $choice -eq 3 ]]; then
    echo "${GREEN}Setting up network with Beckn One${NC}"
    echo "${BLUE}This setup uses Beckn One (DeDI-Registry) for registry and catalog discovery${NC}"
    echo ""
    install_package
    install_beckn_one_adapter
elif [[ $choice -eq 4 ]]; then
    echo "Installing network with local registry and gateway (without Beckn One)"
    install_package
    install_registry
    install_gateway
    install_bap_protocol_server
    install_bpp_protocol_server_with_sandbox
    install_adapter "BOTH"
elif [[ $choice -eq 5 ]]; then
    merge_networks
elif [[ $choice -eq 6 ]]; then
    echo "${BoldGreen}Currently this feature is not available in this distribution of Beckn ONIX${NC}"
    restart_script
elif [[ $choice -eq 7 ]]; then
    update_network
fi

echo "Process complete. Thank you for using Beckn-ONIX!"
