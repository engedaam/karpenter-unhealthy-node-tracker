KO_DOCKER_REPO ?= ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com/dev

image:
	KOCACHE=$(KOCACHE) KO_DOCKER_REPO="$(KO_DOCKER_REPO)" ko build --bare github.com/engedaam/karpenter-unhealthy-node-tracker