#!/bin/bash
set -eo pipefail

GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[0;33m'
RED='\033[0;31m'
NC='\033[0m'

cd "$(dirname "$0")/.."

ENV_FILE=".env"
if [ -f "$ENV_FILE" ]; then
  # shellcheck disable=SC1090
  set -a
  source "$ENV_FILE"
  set +a
fi

PROJECT_ID="${GCP_PROJECT_ID:?GCP_PROJECT_ID is required (or set in .env)}"
REGION="${GCP_REGION:-us-central1}"
SERVICE_NAME="${SERVICE_NAME:-looker-mcp}"
LOOKER_BASE_URL="${LOOKER_BASE_URL:?LOOKER_BASE_URL is required (or set in .env)}"
LOOKER_VERIFY_SSL="${LOOKER_VERIFY_SSL:-true}"
LOOKER_USE_CLIENT_OAUTH="${LOOKER_USE_CLIENT_OAUTH:-false}"
CLOUD_RUN_URL="${CLOUD_RUN_URL:-}"
IMAGE_TAG="gcr.io/${PROJECT_ID}/${SERVICE_NAME}:latest"

echo -e "${BLUE}[INFO]${NC} Targeting GCP Project: ${YELLOW}${PROJECT_ID}${NC}"
echo -e "${BLUE}[INFO]${NC} Targeting Looker Instance: ${YELLOW}${LOOKER_BASE_URL}${NC}"
echo -e "${BLUE}[INFO]${NC} Container Image Tag: ${YELLOW}${IMAGE_TAG}${NC}"

# 1. Compile native Linux binary with CGO
echo -e "${BLUE}[INFO]${NC} Compiling local Linux binary with CGO..."
CGO_ENABLED=1 go build -ldflags "-s -w" -o toolbox-bin .
echo -e "${GREEN}[SUCCESS]${NC} Local binary compiled successfully."

# 2. Build Docker container image
echo -e "${BLUE}[INFO]${NC} Building Docker image..."
docker build -f Dockerfile.local -t "$IMAGE_TAG" .
echo -e "${GREEN}[SUCCESS]${NC} Docker image built."

# 3. Configure Docker auth and push to Container Registry
echo -e "${BLUE}[INFO]${NC} Pushing Docker image to ${IMAGE_TAG}..."
gcloud auth configure-docker gcr.io --quiet
docker push "$IMAGE_TAG"
echo -e "${GREEN}[SUCCESS]${NC} Docker image pushed successfully."

# 4. Deploy to Google Cloud Run
echo -e "${BLUE}[INFO]${NC} Deploying service to Google Cloud Run..."
gcloud run deploy "$SERVICE_NAME" \
  --image "$IMAGE_TAG" \
  --region "$REGION" \
  --project "$PROJECT_ID" \
  --platform managed \
  --allow-unauthenticated \
  --port 8080 \
  --memory 2Gi \
  --set-env-vars="LOOKER_BASE_URL=${LOOKER_BASE_URL},LOOKER_VERIFY_SSL=${LOOKER_VERIFY_SSL},LOOKER_USE_CLIENT_OAUTH=${LOOKER_USE_CLIENT_OAUTH},CLOUD_RUN_URL=${CLOUD_RUN_URL}"

SERVICE_URL=$(gcloud run services describe "$SERVICE_NAME" --region "$REGION" --project "$PROJECT_ID" --format='value(status.url)')
echo -e "${GREEN}[SUCCESS] DEPLOYMENT COMPLETED! 🎉${NC}"
echo -e "Cloud Run URL: ${SERVICE_URL}"
