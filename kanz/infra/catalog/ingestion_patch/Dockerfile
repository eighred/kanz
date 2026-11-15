FROM ghcr.io/eighred/base/datahub-ingestion:v1.8.0-locked@sha256:f46b327ba74574045f5e84f773309ffef47836b3cc00280f3e934bc611677a23
LABEL __KANZ_CATALOG_OWNERSHIP__
USER root
COPY package-lock.json verify_patch.py linux-libc-dev.deb /tmp/kanz-catalog-patch/
# The context has only these reviewed public inputs. No package resolver,
# Python installation, network, credential or mutable base is involved.
RUN printf '%s\n' KANZ_CATALOG_PATCH_PHASE=before-inventory \
    && python /tmp/kanz-catalog-patch/verify_patch.py before \
    && printf '%s\n' KANZ_CATALOG_PATCH_PHASE=package-install \
    && dpkg -i /tmp/kanz-catalog-patch/linux-libc-dev.deb \
    && printf '%s\n' KANZ_CATALOG_PATCH_PHASE=after-inventory \
    && python /tmp/kanz-catalog-patch/verify_patch.py after \
    && rm -rf /tmp/kanz-catalog-patch \
    && printf '%s\n' KANZ_CATALOG_PATCH_PHASE=completed
USER datahub
