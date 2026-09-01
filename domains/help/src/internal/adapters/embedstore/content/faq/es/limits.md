## Para qué sirve esta pestaña
Límites y budget define los techos que protegen tu factura y evitan que un equipo ruidoso perjudique a los demás: rate limit (sol/min) y budget mensual en dólares, por ámbito (org, equipo o app).

## Cómo usarla
Elige el ámbito, define el rate limit y el budget por escalones, y la acción al superar el budget: **alertar**, **degradar** (fuerza el modelo más barato permitido) o **bloquear**. Los cambios valen en el gateway en hasta ~15s.

## Preguntas frecuentes
- **¿Org, equipo o app?** El límite cuenta en el ámbito que lo definió: a nivel org vale para todo; en el equipo, solo para ese equipo.
- **¿Equipo o app — cuál configuro?** Configúralo en el **equipo** por defecto: todas las apps de ese equipo comparten el mismo techo, sin configuración extra. Solo declara un límite directo en una **app** cuando esa app puntual necesite un techo distinto al resto del equipo (p. ej. una app crítica que nunca debe limitarse, o una experimental con techo más ajustado) — el límite de la app sobrescribe el del equipo solo para ella, el resto del equipo no se ve afectado.
- **¿Degradar es seguro?** Mantiene el servicio en pie cambiando a un modelo más barato permitido, en vez de rechazar la solicitud.
