# Build stage — builds natively on the CI runner (amd64), no cross-compile/QEMU.
FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bootstrap .

# Lambda runtime image (provided.al2023 = Amazon Linux 2023 + Lambda Runtime Interface).
# Pinned to x86_64 to match the function's Architectures: [x86_64].
FROM --platform=linux/amd64 public.ecr.aws/lambda/provided:al2023

# Lambda Web Adapter — enables the web function to receive HTTP via Function URL.
# Only the web function sets AWS_LAMBDA_EXEC_WRAPPER=/opt/extensions/lambda-adapter.
COPY --from=public.ecr.aws/awsguru/aws-lambda-adapter:0.8.4 /lambda-adapter /opt/extensions/lambda-adapter

# provided.al2023 execs the custom-runtime bootstrap from LAMBDA_RUNTIME_DIR (/var/runtime).
# The Go binary implements the Runtime API in scan mode (lambda.Start) and runs the HTTP
# server in web mode (wrapped by the adapter).
COPY --from=build /bootstrap /var/runtime/bootstrap
