FROM acryldata/datahub-ingestion:v1.8.0-locked@sha256:f46b327ba74574045f5e84f773309ffef47836b3cc00280f3e934bc611677a23
USER root
COPY package-lock.json verify_patch.py linux-libc-dev.deb /tmp/kanz-catalog-patch/
# The context has only these reviewed public inputs. No package resolver,
# Python installation, network, credential or mutable base is involved.
RUN python /tmp/kanz-catalog-patch/verify_patch.py before \
    && dpkg -i /tmp/kanz-catalog-patch/linux-libc-dev.deb \
    && python /tmp/kanz-catalog-patch/verify_patch.py after \
    && rm -rf /tmp/kanz-catalog-patch
USER datahub
