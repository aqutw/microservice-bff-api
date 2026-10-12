FROM node:20-alpine
WORKDIR /app
COPY package.json ./
COPY src ./src
ENV NODE_ENV=production
EXPOSE 3001 3002
CMD ["node", "src/desktop.js"]
