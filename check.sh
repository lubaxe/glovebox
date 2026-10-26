#! /bin/bash

# print shellcheck version so we know what Github uses
shellcheck -V

# SC1091 - Don't complain about not being able to find files that don't exist.
shellcheck --exclude=SC1091 --source-path=./internal/bootfs/assets \
           ./pi-gen-sources/build-image.sh \
           ./pi-gen-sources/prepare.sh \
           ./pi-gen-sources/00-glovebox/00-run.sh \
           ./pi-gen-sources/00a-nodoc/00-run.sh \
           ./scripts/install.sh \
           ./internal/bootfs/assets/glovebox-boot-maintenance \
           ./scripts/publish.sh
