FROM node:20-alpine AS builder

RUN corepack enable && corepack prepare pnpm@10.29.3 --activate

WORKDIR /app

COPY pnpm-workspace.yaml package.json pnpm-lock.yaml* ./
COPY tsconfig.json vite.config.ts tailwind.config.js postcss.config.js ./
COPY app/ ./app/

RUN pnpm install --frozen-lockfile=false
RUN pnpm run build

FROM node:20-alpine AS runtime

RUN corepack enable && corepack prepare pnpm@10.29.3 --activate

WORKDIR /app

COPY --from=builder /app/package.json /app/pnpm-workspace.yaml ./
COPY --from=builder /app/pnpm-lock.yaml* ./
RUN pnpm install --prod --frozen-lockfile=false

COPY --from=builder /app/build ./build

EXPOSE 3000
ENV NODE_ENV=production

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q --spider http://localhost:3000/ || exit 1

CMD ["node_modules/.bin/remix-serve", "build/server/index.js"]
