FROM golang:1.26-bookworm AS unit
ENV GOTOOLCHAIN=local GOMAXPROCS=2 CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api api
COPY cmd cmd
COPY internal internal
RUN go build ./cmd/... ./internal/...
COPY integrations integrations
COPY config config
COPY charts charts
COPY hack hack
COPY test test
RUN go test -exec=/bin/true ./...

FROM unit AS charts
COPY --from=alpine/helm:3.19.0 /usr/bin/helm /usr/local/bin/helm
COPY Makefile .

FROM charts AS integration
RUN CGO_ENABLED=0 go build -o /fixtures/manager/manager ./cmd && \
    CGO_ENABLED=0 go build -o /fixtures/fake/fakebackend ./test/fakebackend && \
    cp test/e2e/testdata/manager.Dockerfile /fixtures/manager/Dockerfile && \
    cp test/e2e/testdata/fake.Dockerfile /fixtures/fake/Dockerfile && \
    touch /etc/tmo-isolated-runner

FROM integration AS argo
ADD https://raw.githubusercontent.com/argoproj/argo-cd/v3.3.2/manifests/core-install.yaml /fixtures/argocd-core.yaml
RUN echo 'b7d4d583e2a79a19f190d8d9ed29b8dd9ed3b2c0b9017810b68703c391a2bf2f  /fixtures/argocd-core.yaml' | sha256sum -c -
