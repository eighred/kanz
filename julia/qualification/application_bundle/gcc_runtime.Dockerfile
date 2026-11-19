FROM scratch
ADD gcc-musl-root.tar.gz /
ADD gcc-gnu-platform.tar.gz /opt/x86_64-linux-gnu/
ADD gcc-gnu-bootstrap.tar.gz /opt/x86_64-linux-gnu/
ENV PATH=/opt/x86_64-linux-gnu/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
USER 65532:65532
ENTRYPOINT ["/bin/bash"]
