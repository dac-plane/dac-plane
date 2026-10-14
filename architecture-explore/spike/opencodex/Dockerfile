FROM node:22-slim
RUN npm install -g @bitkyc08/opencodex@2.84.0
RUN useradd -m -u 10001 opencodex && mkdir -p /opt/opencodex && chown -R opencodex:opencodex /opt/opencodex && chmod 700 /opt/opencodex
COPY entrypoint.sh /usr/local/bin/ocx-entrypoint
RUN chmod 755 /usr/local/bin/ocx-entrypoint
USER opencodex
ENV OPENCODEX_HOME=/opt/opencodex
EXPOSE 10100
ENTRYPOINT ["/usr/local/bin/ocx-entrypoint"]
