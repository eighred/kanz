FROM scratch
ADD gcc-musl-root.tar.gz /
ADD gcc-gnu-platform.tar.gz /opt/x86_64-linux-gnu/
ADD gcc-gnu-bootstrap.tar.gz /opt/x86_64-linux-gnu/
ENV PATH=/opt/x86_64-linux-gnu/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
# llvm-config derives its prefix from its own executable and requires the target GNU loader.
RUN test -f /lib64/ld-linux-x86-64.so.2 && ln -sf /opt/x86_64-linux-gnu/x86_64-linux-gnu/sys-root/lib64/ld-linux-x86-64.so.2 /lib64/ld-linux-x86-64.so.2
USER 65532:65532
ENTRYPOINT ["/bin/bash"]
