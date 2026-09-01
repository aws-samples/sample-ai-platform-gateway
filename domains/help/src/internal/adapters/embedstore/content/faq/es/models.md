## Para qué sirve esta pestaña
Modelos y enrutamiento es donde conectas proveedores y modelos, defines el orden de prioridad (predeterminado + fallback), y activas cache y auto-cheapest. Cada modelo es una **ruta**: un alias que tu código llama y que apunta a un proveedor — cambiar el proveedor detrás no cambia una línea de tu app.

## Cómo usarla
En la sub-pestaña **Modelos**, agrega proveedores y activa o desactiva cada modelo (etiquetas 🌐 externo / 🏠 interno). En **Enrutamiento**, arrastra para ordenar los modelos activos. En **Configuración**, activa auto-cheapest y los caches. El precio viene precargado de la lista pública; si tienes descuento, edítalo al tuyo.

## Preguntas frecuentes
- **¿Externo o interno?** Externo = proveedor SaaS (incluye Bedrock, que corre en tu propia cuenta). Interno = endpoint propio (self-host).
- **¿El auto-cheapest ignora mi orden?** Activado, el orden de intento pasa a ser por precio (más barato → siguiente), respetando la elegibilidad (tool use, imagen, ventana de contexto).

## Cache exacto vs cache semántico

Son dos mecanismos distintos, y la diferencia importa.

**Cache exacto** guarda la respuesta y la devuelve cuando vuelve exactamente la misma pregunta. El ahorro es **comprobado**: la llamada al proveedor simplemente no ocurrió. No tiene riesgo.

**Cache semántico** va más allá y devuelve la respuesta de una pregunta *parecida*, comparando el significado. Cambia tanto el perfil de costo como el de riesgo:

- **Costo de latencia:** agrega ~60 ms en las solicitudes que **no** aciertan, porque la pregunta debe convertirse en vector.
- **Ganancia:** cada acierto evita **100% del costo** de esa llamada. Cuánto reduce tu factura depende de tu tasa de acierto, que sigues en **ROI y ahorro**.
- **Riesgo:** la respuesta es **aproximada, no idéntica**. Admite falsos positivos.

Por eso viene desactivado y su ahorro aparece separado del ahorro comprobado en ROI.

### Cuándo NO usar cache semántico
Si tus preguntas se distinguen por **detalle fino**, el mecanismo tiende a perjudicar más que a ayudar. Ejemplos donde la diferencia textual es pequeña y la respuesta correcta es muy distinta: una pregunta sobre un equipo vs otro, un período vs otro, una función vs otra parecida.

Una protección ya es automática: las preguntas con **números diferentes nunca coinciden** entre sí (60 y 600 no se confunden), porque la comparación por significado es débil justamente con cantidades.

### Rigor (threshold)
El rigor define cuánto deben parecerse dos preguntas para coincidir. Más alto = menos aciertos, menos riesgo. Más bajo = más aciertos, más probabilidad de una respuesta aproximada equivocada. El valor por defecto es conservador a propósito.
