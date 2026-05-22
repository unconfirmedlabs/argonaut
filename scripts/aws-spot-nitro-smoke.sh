#!/usr/bin/env bash
# Launch a temporary spot EC2 instance, run argonaut's Nitro smoke test, and
# tear everything down.
#
# Intended for CI. Requires AWS credentials in the environment or an AWS profile.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-}}"
INSTANCE_TYPE="${ARGONAUT_CI_INSTANCE_TYPE:-c5.xlarge}"
INSTANCE_NAME="${ARGONAUT_CI_INSTANCE_NAME:-argonaut-nitro-smoke}"
AMI_SSM_PATH="${ARGONAUT_CI_AMI_SSM_PATH:-/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-6.1-x86_64}"
REMOTE_DIR="${ARGONAUT_CI_REMOTE_DIR:-/home/ec2-user/argonaut}"
RUN_COMMAND="${ARGONAUT_CI_RUN_COMMAND:-ARGONAUT_SMOKE_KEEP_WORKDIR=1 scripts/nitro-smoke.sh}"
FETCH_PATH="${ARGONAUT_CI_FETCH_PATH:-}"
ARTIFACT_DIR="${ARGONAUT_CI_ARTIFACT_DIR:-$ROOT/.argonaut-ci-artifacts/$(date +%Y%m%d-%H%M%S)}"
ALLOCATOR_CPUS="${ARGONAUT_CI_ALLOCATOR_CPUS:-${ARGONAUT_BENCH_CPUS:-2}}"
ALLOCATOR_MEMORY="${ARGONAUT_CI_ALLOCATOR_MEMORY:-${ARGONAUT_BENCH_MEMORY:-1024}}"
KEEP_INSTANCE="${ARGONAUT_CI_KEEP_INSTANCE:-}"
KEEP_SECURITY_GROUP="${ARGONAUT_CI_KEEP_SECURITY_GROUP:-}"
KEEP_KEY_PAIR="${ARGONAUT_CI_KEEP_KEY_PAIR:-}"
SSH_CIDR="${ARGONAUT_CI_SSH_CIDR:-}"
SPOT_MAX_PRICE="${ARGONAUT_CI_SPOT_MAX_PRICE:-}"
SUBNET_ID="${ARGONAUT_CI_SUBNET_ID:-}"
SECURITY_GROUP_ID="${ARGONAUT_CI_SECURITY_GROUP_ID:-}"
KEY_NAME="${ARGONAUT_CI_KEY_NAME:-}"
PRIVATE_KEY_FILE="${ARGONAUT_CI_PRIVATE_KEY_FILE:-}"

INSTANCE_ID=""
CREATED_SECURITY_GROUP_ID=""
CREATED_KEY_NAME=""
TEMP_KEY_FILE=""
USER_DATA_FILE=""
WORKDIR=""

log() {
  echo "[aws-smoke] $*"
}

die() {
  echo "[aws-smoke] ERROR: $*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

cleanup() {
  set +e
  if [[ -n "$INSTANCE_ID" && -z "$KEEP_INSTANCE" ]]; then
    log "terminating instance $INSTANCE_ID"
    aws ec2 terminate-instances --instance-ids "$INSTANCE_ID" --region "$REGION" >/dev/null 2>&1
    aws ec2 wait instance-terminated --instance-ids "$INSTANCE_ID" --region "$REGION" >/dev/null 2>&1
  elif [[ -n "$INSTANCE_ID" ]]; then
    log "keeping instance $INSTANCE_ID"
  fi

  if [[ -n "$CREATED_SECURITY_GROUP_ID" && -z "$KEEP_SECURITY_GROUP" ]]; then
    log "deleting security group $CREATED_SECURITY_GROUP_ID"
    aws ec2 delete-security-group --group-id "$CREATED_SECURITY_GROUP_ID" --region "$REGION" >/dev/null 2>&1
  fi

  if [[ -n "$CREATED_KEY_NAME" && -z "$KEEP_KEY_PAIR" ]]; then
    log "deleting key pair $CREATED_KEY_NAME"
    aws ec2 delete-key-pair --key-name "$CREATED_KEY_NAME" --region "$REGION" >/dev/null 2>&1
  fi

  [[ -n "$TEMP_KEY_FILE" ]] && rm -f "$TEMP_KEY_FILE"
  [[ -n "$USER_DATA_FILE" ]] && rm -f "$USER_DATA_FILE"
  [[ -n "$WORKDIR" ]] && rm -rf "$WORKDIR"
}
trap cleanup EXIT

aws_text() {
  aws "$@" --region "$REGION" --output text
}

archive_repo() {
	local out="$1"
	if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 &&
		git -C "$ROOT" rev-parse --verify HEAD >/dev/null 2>&1; then
    git -C "$ROOT" archive --format=tar HEAD > "$out"
    local status
    status="$(git -C "$ROOT" status --short)"
    if [[ -n "$status" ]]; then
      log "repo has uncommitted changes; overlaying working tree into archive"
    COPYFILE_DISABLE=1 tar --exclude='.git' --exclude='.argonaut-ci-artifacts' --exclude='argonaut' --exclude='*.test' --exclude='coverage.out' \
        -C "$ROOT" -rf "$out" .
    fi
  else
    COPYFILE_DISABLE=1 tar --exclude='.git' --exclude='.argonaut-ci-artifacts' --exclude='argonaut' --exclude='*.test' --exclude='coverage.out' \
      -C "$ROOT" -cf "$out" .
  fi
}

wait_for_ssh() {
  local public_ip="$1"
  local ssh_cmd="$2"
  log "waiting for SSH at $public_ip"
  for _ in $(seq 1 60); do
    if $ssh_cmd "ec2-user@$public_ip" "echo ready" >/dev/null 2>&1; then
      return 0
    fi
    sleep 10
  done
  return 1
}

wait_for_cloud_init() {
  local public_ip="$1"
  local ssh_cmd="$2"
  log "waiting for instance setup"
  for _ in $(seq 1 60); do
    if $ssh_cmd "ec2-user@$public_ip" "test -f /tmp/argonaut-ci-ready" >/dev/null 2>&1; then
      return 0
    fi
    sleep 10
  done
  $ssh_cmd "ec2-user@$public_ip" "sudo cat /var/log/cloud-init-output.log" || true
  return 1
}

need_cmd aws
need_cmd ssh
need_cmd scp
need_cmd tar

[[ -n "$REGION" ]] || die "set AWS_REGION or AWS_DEFAULT_REGION"

WORKDIR="$(mktemp -d)"
SHORT_ID="$(date +%s)-$$"

log "resolving Amazon Linux 2023 AMI"
AMI_ID="$(aws_text ssm get-parameter --name "$AMI_SSM_PATH" --query 'Parameter.Value')"
[[ -n "$AMI_ID" && "$AMI_ID" != "None" ]] || die "could not resolve AMI from $AMI_SSM_PATH"
log "AMI: $AMI_ID"

if [[ -z "$SUBNET_ID" ]]; then
  log "using a default subnet"
  SUBNET_ID="$(aws_text ec2 describe-subnets \
    --filters Name=default-for-az,Values=true Name=state,Values=available \
    --query 'Subnets[0].SubnetId')"
  [[ -n "$SUBNET_ID" && "$SUBNET_ID" != "None" ]] || die "no default subnet found; set ARGONAUT_CI_SUBNET_ID"
fi

VPC_ID="$(aws_text ec2 describe-subnets --subnet-ids "$SUBNET_ID" --query 'Subnets[0].VpcId')"
[[ -n "$VPC_ID" && "$VPC_ID" != "None" ]] || die "could not resolve VPC for subnet $SUBNET_ID"
log "subnet: $SUBNET_ID vpc: $VPC_ID"

if [[ -z "$KEY_NAME" ]]; then
  KEY_NAME="argonaut-ci-${SHORT_ID}"
  TEMP_KEY_FILE="$WORKDIR/$KEY_NAME.pem"
  log "creating temporary key pair $KEY_NAME"
  aws_text ec2 create-key-pair \
    --key-name "$KEY_NAME" \
    --key-type ed25519 \
    --query 'KeyMaterial' > "$TEMP_KEY_FILE"
  chmod 600 "$TEMP_KEY_FILE"
  CREATED_KEY_NAME="$KEY_NAME"
  PRIVATE_KEY_FILE="$TEMP_KEY_FILE"
else
  [[ -n "$PRIVATE_KEY_FILE" ]] || die "ARGONAUT_CI_PRIVATE_KEY_FILE is required when ARGONAUT_CI_KEY_NAME is set"
fi

if [[ -z "$SSH_CIDR" ]]; then
  if command -v curl >/dev/null 2>&1; then
    PUBLIC_RUNNER_IP="$(curl -fsS https://checkip.amazonaws.com 2>/dev/null | tr -d '[:space:]' || true)"
    if [[ -n "$PUBLIC_RUNNER_IP" ]]; then
      SSH_CIDR="$PUBLIC_RUNNER_IP/32"
    fi
  fi
fi
SSH_CIDR="${SSH_CIDR:-0.0.0.0/0}"

if [[ -z "$SECURITY_GROUP_ID" ]]; then
  SG_NAME="argonaut-ci-${SHORT_ID}"
  log "creating temporary security group $SG_NAME with SSH ingress $SSH_CIDR"
  SECURITY_GROUP_ID="$(aws_text ec2 create-security-group \
    --group-name "$SG_NAME" \
    --description "temporary argonaut Nitro smoke test" \
    --vpc-id "$VPC_ID" \
    --query 'GroupId')"
  CREATED_SECURITY_GROUP_ID="$SECURITY_GROUP_ID"
  aws ec2 authorize-security-group-ingress \
    --group-id "$SECURITY_GROUP_ID" \
    --ip-permissions "IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=$SSH_CIDR,Description=argonaut-ci-ssh}]" \
    --region "$REGION" >/dev/null
fi
log "security group: $SECURITY_GROUP_ID"

USER_DATA_FILE="$WORKDIR/user-data.sh"
cat > "$USER_DATA_FILE" <<USERDATA
#!/bin/bash
set -euxo pipefail
dnf install -y aws-nitro-enclaves-cli aws-nitro-enclaves-cli-devel docker git golang jq tar gzip
systemctl enable --now docker
usermod -aG docker ec2-user
usermod -aG ne ec2-user
sed -i 's/^memory_mib:.*/memory_mib: $ALLOCATOR_MEMORY/' /etc/nitro_enclaves/allocator.yaml
sed -i 's/^cpu_count:.*/cpu_count: $ALLOCATOR_CPUS/' /etc/nitro_enclaves/allocator.yaml
systemctl enable --now nitro-enclaves-allocator
touch /tmp/argonaut-ci-ready
USERDATA

MARKET_OPTIONS='MarketType=spot,SpotOptions={SpotInstanceType=one-time,InstanceInterruptionBehavior=terminate}'
if [[ -n "$SPOT_MAX_PRICE" ]]; then
  MARKET_OPTIONS="MarketType=spot,SpotOptions={SpotInstanceType=one-time,InstanceInterruptionBehavior=terminate,MaxPrice=$SPOT_MAX_PRICE}"
fi

log "launching one-time spot $INSTANCE_TYPE"
INSTANCE_ID="$(aws_text ec2 run-instances \
  --image-id "$AMI_ID" \
  --instance-type "$INSTANCE_TYPE" \
  --key-name "$KEY_NAME" \
  --security-group-ids "$SECURITY_GROUP_ID" \
  --subnet-id "$SUBNET_ID" \
  --associate-public-ip-address \
  --enclave-options 'Enabled=true' \
  --instance-market-options "$MARKET_OPTIONS" \
  --metadata-options 'HttpTokens=required,HttpEndpoint=enabled' \
  --user-data "file://$USER_DATA_FILE" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$INSTANCE_NAME},{Key=Project,Value=argonaut},{Key=Purpose,Value=nitro-smoke}]" \
  --query 'Instances[0].InstanceId')"
[[ -n "$INSTANCE_ID" && "$INSTANCE_ID" != "None" ]] || die "run-instances did not return an instance ID"
log "instance: $INSTANCE_ID"

aws ec2 wait instance-running --instance-ids "$INSTANCE_ID" --region "$REGION"
PUBLIC_IP="$(aws_text ec2 describe-instances \
  --instance-ids "$INSTANCE_ID" \
  --query 'Reservations[0].Instances[0].PublicIpAddress')"
[[ -n "$PUBLIC_IP" && "$PUBLIC_IP" != "None" ]] || die "instance has no public IP"
log "public IP: $PUBLIC_IP"

SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -i "$PRIVATE_KEY_FILE")
SSH=(ssh "${SSH_OPTS[@]}")
SCP=(scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$PRIVATE_KEY_FILE")

wait_for_ssh "$PUBLIC_IP" "${SSH[*]}" || die "SSH timeout"
wait_for_cloud_init "$PUBLIC_IP" "${SSH[*]}" || die "instance setup timeout"

ARCHIVE="$WORKDIR/argonaut.tar"
archive_repo "$ARCHIVE"

log "copying repo archive"
"${SSH[@]}" "ec2-user@$PUBLIC_IP" "rm -rf '$REMOTE_DIR' && mkdir -p '$REMOTE_DIR'"
"${SCP[@]}" "$ARCHIVE" "ec2-user@$PUBLIC_IP:$REMOTE_DIR/argonaut.tar"
"${SSH[@]}" "ec2-user@$PUBLIC_IP" "cd '$REMOTE_DIR' && tar -xf argonaut.tar && rm argonaut.tar && chmod +x scripts/nitro-smoke.sh scripts/nitro-bench.sh"

log "running on instance: $RUN_COMMAND"
"${SSH[@]}" "ec2-user@$PUBLIC_IP" "cd '$REMOTE_DIR' && $RUN_COMMAND"

if [[ -n "$FETCH_PATH" ]]; then
  log "fetching remote artifact path $FETCH_PATH to $ARTIFACT_DIR"
  mkdir -p "$ARTIFACT_DIR"
  "${SCP[@]}" -r "ec2-user@$PUBLIC_IP:$REMOTE_DIR/$FETCH_PATH" "$ARTIFACT_DIR/"
fi

log "PASS: command completed on $INSTANCE_ID"
