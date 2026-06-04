#
# Build
#

FROM registry.redhat.io/openshift4/ose-tools-rhel9:v4.20.0-202602040619.p2.g040daf8.assembly.stream.el9 AS ose-tools
FROM registry.access.redhat.com/ubi10/go-toolset:1.25.7-1772050924 AS builder

ARG COMMIT_ID
ARG VERSION_ID

USER root
WORKDIR /workdir/tsf

COPY installer/ ./installer/

COPY cmd/ ./cmd/
COPY vendor/ ./vendor/

COPY go.mod go.sum Makefile ./

RUN make GOFLAGS='-buildvcs=false' COMMIT_ID=${COMMIT_ID} VERSION=${VERSION_ID}

#
# Run
#

FROM registry.access.redhat.com/ubi10:10.2-1780550529

LABEL \
  name="tsf" \
  com.redhat.component="tsf" \
  description="Red Hat Trusted Software Factory (TSF) Installer is an opinionated distribution including \
    the Konflux build system, designed to deliver a secure, \"security-built-in\" software supply chain. \
    It shifts security left by automating compliance, provenance verification (SLSA Level 3), and SBOM \
    generation within the build pipeline, rather than treating them as external gates." \
  io.k8s.description="Red Hat Trusted Software Factory (TSF) Installer is an opinionated distribution including \
    the Konflux build system, designed to deliver a secure, \"security-built-in\" software supply chain. \
    It shifts security left by automating compliance, provenance verification (SLSA Level 3), and SBOM \
    generation within the build pipeline, rather than treating them as external gates." \
  summary="Provides the tsf binary." \
  io.k8s.display-name="Red Hat Trusted Software Factory CLI" \
  io.openshift.tags="ec konflux openshift tas tpa tsf"

# Banner
RUN echo 'cat << "EOF"' >> /etc/profile && \
    echo '╔═══════════════════════════════════════════════════════╗' >> /etc/profile && \
    echo '║   Welcome to the Trusted Software Factory Installer   ║' >> /etc/profile && \
    echo '╚═══════════════════════════════════════════════════════╝' >> /etc/profile && \
    echo ' ' >> /etc/profile && \
    echo 'To deploy the Trusted Software Factory:' >> /etc/profile && \
    echo '  - Login to the cluster' >> /etc/profile && \
    echo '  - Create the TSF config on the cluster' >> /etc/profile && \
    echo '  - Create the integrations' >> /etc/profile && \
    echo '  - Deploy TSF' >> /etc/profile && \
    echo ' ' >> /etc/profile && \
    echo 'For more information, please visit https://github.com/redhat-appstudio/tsf-cli/blob/main/docs/trusted-software-factory.md' >> /etc/profile && \
    echo ' ' >> /etc/profile && \
    echo '!!! DISCLAIMER: ONLY FOR EXPERIMENTAL DEPLOYMENTS - PRODUCTION IS UNSUPPORTED !!!' >> /etc/profile && \
    echo ' ' >> /etc/profile && \
    echo 'EOF' >> /etc/profile

WORKDIR /licenses

COPY LICENSE.txt .

WORKDIR /tsf

COPY --from=ose-tools /usr/bin/jq /usr/bin/kubectl /usr/bin/oc /usr/bin/vi /usr/bin/
# jq libraries
COPY --from=ose-tools /usr/lib64/libjq.so.1 /usr/lib64/libonig.so.5 /usr/lib64/
# vi libraries
COPY --from=ose-tools /usr/libexec/vi /usr/libexec/

COPY --from=builder /workdir/tsf/installer/charts ./charts
COPY --from=builder /workdir/tsf/installer/config.yaml ./
COPY --from=builder /workdir/tsf/bin/tsf /usr/local/bin/tsf

COPY scripts ./scripts
RUN chmod +x ./scripts/*.sh

RUN groupadd --gid 9999 -r tsf && \
    useradd -r -d /tsf -g tsf -s /sbin/nologin --uid 9999 tsf && \
    chown -R tsf:tsf .

USER tsf

RUN echo "# jq" && jq --version && \
    echo "# kubectl" && kubectl version --client && \
    echo "# oc" && oc version

ENV KUBECONFIG=/tsf/.kube/config
ENV TSF_NO_DISCLAIMER=""

ENTRYPOINT ["tsf"]
