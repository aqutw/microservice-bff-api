FROM node:22-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci --omit=dev --ignore-scripts
COPY src ./src
ENV NODE_ENV=production
EXPOSE 3001 3002
CMD ["node", "src/desktop.js"]
