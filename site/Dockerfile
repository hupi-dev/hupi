# Optional, self-hosted deployment path for the marketing site — see this
# directory's README.md's "Deploying" section. Vercel/Netlify remain the
# simplest options for most people; this exists for anyone who'd rather run
# the site themselves (e.g. alongside the rest of a self-hosted HUPI
# deployment). Independent of the main repo's Dockerfile/deploy/ — this
# site has no dependency on Postgres or anything else HUPI needs.

# ---- build stage ----
FROM node:22-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

# ---- runtime stage ----
FROM nginx:alpine
COPY --from=build /src/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
