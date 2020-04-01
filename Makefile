NAME:=vertica_exporter
COMMIT := $(shell git log -1 --format=%ct)
DESCRIPTION:="Prometheus vertica exporter"
MAINTAINER:="Sergey Ignatov <sergey.a.ignatov@gmail.com>"
VERSION ?= 0



all: bin/$(NAME)
bin/$(NAME): deps
	go build -ldflags "-X main.revision=$(VERSION)" -o bin/$(NAME)



test:
	@go test ./...
clean:
	rm -rf bin
deb: bin/$(NAME)
	fpm -s dir -t deb -n $(NAME) -v $(VERSION) \
    		--deb-priority optional --category admin \
    		--force \
			--url https://github.com/sergeyignatov/$(NAME) \
    		--description $(DESCRIPTION) \
    		-m $(MAINTAINER) \
    		--license "MIT" \
    		-a x86_64 \
    		--before-install misc/vertica_exporter.preinst \
			--config-files /etc/default/$(NAME) \
			--config-files /lib/systemd/system/$(NAME).service \
			misc/$(NAME).default=/etc/default/$(NAME) \
			misc/$(NAME).service=/lib/systemd/system/$(NAME).service \
			bin/$(NAME)=/usr/bin/$(NAME)


gomodcheck:
	@go help mod > /dev/null || (@echo micromdm requires Go version 1.11 or higher && exit 1)

deps: gomodcheck
	@go mod download

lint: bin/golangci-lint
	./bin/golangci-lint run ./...

bin/golangci-lint:
	@echo "Installing golangci-lint"
	@curl -sfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh| sh