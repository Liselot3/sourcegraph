# This image creates a standalone web server for demoing the prototype
FROM node:16@sha256:8c3c7e253d9484a35ed9db3e6ba296d859a382f48602d0321e4734a91311ac95

ARG PROJECT_ROOT="."

WORKDIR /usr/src/app
RUN echo '{"type": "module"}' > package.json
RUN npm install express@~4.18 http-proxy-middleware@~2.0
COPY  ${PROJECT_ROOT}/server.js ./
COPY ${PROJECT_ROOT}/build ./build

ENV SOURCEGRAPH_API_HOST=https://sourcegraph.com
EXPOSE 4173
CMD ["node", "server.js"]
