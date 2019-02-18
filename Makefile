LENSVERSION=`git describe --tags`
EDITION=cpu
GOFLAGS=
DIST=$(shell uname)
ifeq ($(DIST), Linux) 
GOFLAGS=-tags gcc7
endif

lens:
	@make cli

.PHONY: deps
deps: 
	@echo "=================== generating dependencies ==================="
	# Install tensorflow
	bash setup/scripts/tensorflow_install.sh

	# Install tesseract
	bash setup/scripts/tesseract_install.sh

	# Update standard dependencies
	dep ensure -v

	# install gofitz
	go get -u $(GOFLAGS) github.com/gen2brain/go-fitz

	# Install counterfeiter, used for mock generation
	go get -u github.com/maxbrunsfeld/counterfeiter
	@echo "===================          done           ==================="

# Build lens cli
.PHONY: cli
cli:
	@echo "====================  building Lens CLI  ======================"
	rm -f temporal-lens
	go build $(GOFLAGS) \
		-ldflags "-X main.Version=$(LENSVERSION) -X main.Edition=$(EDITION)" \
		./cmd/temporal-lens
	@echo "===================          done           ==================="

# Set up test environment
.PHONY: testenv
WAIT=3
testenv:
	@echo "===================   preparing test env    ==================="
	docker-compose -f test/docker-compose.yml up -d
	sleep $(WAIT)
	@echo "===================          done           ==================="

.PHONY: testenv-integration
testenv-integration: testenv
	@echo Connecting testenv IPFS node to RTrade IPFS node for test assets
	ipfs --api=/ip4/127.0.0.1/tcp/5001 swarm connect /ip4/172.218.49.115/tcp/5002/ipfs/Qmf964tiE9JaxqntDsSBGasD4aaofPQtfYZyMSJJkRrVTQ

# Run simple checks
.PHONY: check
check:
	go vet $(GOFLAGS) ./...
	go test $(GOFLAGS) -run xxxx ./...

# Generate code
.PHONY: gen
gen:
	ifacemaker -d true \
		-f search/search.go \
		-s Service \
		-i Searcher \
		--pkg search \
		-o search/search.i.go \
		-c "Code generated by ifacemaker. DO NOT EDIT." \
		-y "Searcher provides the internal Lens search API"
	counterfeiter -o ./mocks/search.mock.go \
		./search/search.i.go Searcher
	counterfeiter -o ./mocks/manager.mock.go \
		-fake-name FakeRTFSManager \
		./vendor/github.com/RTradeLtd/rtfs/rtfs.i.go Manager
	counterfeiter -o ./mocks/images.mock.go \
		./analyzer/images/tensorflow.go TensorflowAnalyzer
	counterfeiter -o ./mocks/engine.mock.go \
		-fake-name FakeEngineSearcher \
		./engine/engine.go Searcher

# Build docker release
.PHONY: docker
docker:
	@echo "===================  building docker image  ==================="
	@echo EDITION: $(EDITION)
	@docker build \
		--build-arg LENSVERSION=$(LENSVERSION)-$(EDITION) \
		--build-arg TENSORFLOW_DIST=$(EDITION) \
		-t rtradetech/lens:$(LENSVERSION)-$(EDITION) .
	@echo "===================          done           ==================="

.PHONY: v2
v2: cli
	./temporal-lens --dev --cfg test/config.json v2
