## Para qué sirve esta pestaña
Alertas define reglas evaluadas contra el uso real (gasto, latencia, cache bajo, tasa de error, capacidad del proveedor) y un webhook para recibir el aviso cuando una regla dispara.

## Cómo usarla
Activa las reglas que importan y define el umbral de cada una. Informa un webhook (una URL tuya) para recibir el POST. Un evaluador corre en el servidor cada 15 min y entrega al webhook, como máximo una vez por regla por día.

## Preguntas frecuentes
- **¿La regla de consumo del plan se entrega?** Se evalúa en vivo en esta pantalla, pero todavía no se entrega por webhook.
- **¿Por qué no disparó?** Las reglas de error tienen un piso de volumen por ventana, para evitar ruido cuando hay pocas solicitudes.

## Cómo funciona la entrega (y qué todavía no se entrega)

Un evaluador corre en el servidor cada **15 minutos**, revisa las reglas activas y hace un POST a tu webhook cuando alguna dispara. El disparo se limita a **una vez por regla por día** (cooldown), para que un problema continuo no se convierta en una avalancha de avisos.

**Entregados por webhook:** gasto del mes, latencia promedio, cache bajo, tasa de error, capacidad del proveedor, quema de error budget (SLO) y anomalía versus tu propio historial.

**Evaluada solo en vivo en esta pantalla:** consumo de la cuota del plan. Depende del catálogo de planes, que es de otro dominio, y todavía no se entrega por webhook.

## Un alerta puede disparar y no llegar

Si tu webhook está caído o rechaza la llamada, el aviso se pierde — y **no se reenvía**, porque el cooldown del día ya se consumió.

Por eso existe el historial en **Registros → Alertas**: ahí ves cada disparo, el valor que lo activó y si la entrega funcionó. Si dice "no entregado", revisa la URL del webhook aquí. Guardamos solo el host de la dirección, nunca la URL completa, porque suele llevar un token.
