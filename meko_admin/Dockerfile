FROM node:22-alpine AS builder

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build

# ---- production image ----
FROM node:22-alpine

WORKDIR /app

COPY --from=builder /app/dist ./dist

EXPOSE 3011

ENV NODE_ENV=production

CMD ["node", "dist/index.cjs"]
