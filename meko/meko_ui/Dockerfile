# Base image used by all stages
FROM node:22-alpine AS base
WORKDIR /app

# Install all deps once (dev + prod) for building
FROM base AS deps
COPY package.json package-lock.json ./
RUN npm ci

# Build frontend + bundled backend into /dist
FROM deps AS build
ARG VITE_COGNITO_DOMAIN
ARG VITE_COGNITO_CLIENT_ID
ARG VITE_COGNITO_REDIRECT_URI
ARG VITE_OBSERVE_EMBED_PATH
ARG VITE_INFERENCE_GATEWAY_URL
ENV VITE_COGNITO_DOMAIN=$VITE_COGNITO_DOMAIN
ENV VITE_COGNITO_CLIENT_ID=$VITE_COGNITO_CLIENT_ID
ENV VITE_COGNITO_REDIRECT_URI=$VITE_COGNITO_REDIRECT_URI
ENV VITE_OBSERVE_EMBED_PATH=$VITE_OBSERVE_EMBED_PATH
ENV VITE_INFERENCE_GATEWAY_URL=$VITE_INFERENCE_GATEWAY_URL
# RudderStack telemetry is configured at RUNTIME (not baked in here): the server
# serves window.__MEKO_RUNTIME__ from the pod env, so the write key + data-plane
# URL come from the K8s secret / deployment env at deploy time.
COPY . .
RUN npm run build

# Minimal runtime image
FROM node:22-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV PORT=5000

# Install only production dependencies
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force

# Copy build artifacts from build stage
COPY --from=build /app/dist ./dist

EXPOSE 5000
# Start the production server bundle
CMD ["npm", "run", "start"]
