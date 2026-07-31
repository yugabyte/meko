# syntax=docker/dockerfile:1
#
# amp_ui production image: build the React/Vite SPA to static assets, serve them
# with nginx, and reverse-proxy /api/ to the amp controller so the SPA stays
# same-origin (no CORS, no per-environment rebuild).
#
# Build (context = this directory):
#   docker build -t <repo>/amp/controller-ui:<tag> amp_ui
#
# Runtime env (templated into the nginx config at container start):
#   CONTROLLER_URL  controller REST API origin /api/ is proxied to. Default is
#                   the in-namespace controller Service (http://amp-controller:8080);
#                   the yb-amp-controller chart passes the fully-qualified
#                   Service DNS. Must be an origin with no trailing path.
#   UI_PORT         port nginx listens on (default 80).

# --- build stage: compile the SPA ---
FROM node:22-bookworm-slim AS build
WORKDIR /app

# Install deps first so source-only changes reuse this layer.
COPY package.json package-lock.json ./
RUN npm ci

COPY . .
# `npm run build` is `vite build` (no tsc), so a type error won't fail the image.
RUN npm run build

# --- runtime stage: nginx serving the static build ---
FROM nginx:1.27-alpine AS runtime

# The nginx image entrypoint runs envsubst over /etc/nginx/templates/*.template
# → /etc/nginx/conf.d/ before starting nginx. Naming this default.conf.template
# makes it replace the stock default.conf (no listen-port clash).
COPY nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html

# NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1 tells the image entrypoint to fill
# NGINX_LOCAL_RESOLVERS from /etc/resolv.conf (the cluster DNS) before envsubst,
# so the template's `resolver` directive can defer upstream DNS to request time.
ENV CONTROLLER_URL=http://amp-controller:8080 \
    UI_PORT=80 \
    NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1

EXPOSE 80

# Inherits the nginx image's ENTRYPOINT (envsubst on templates) + CMD
# (nginx -g "daemon off;").
