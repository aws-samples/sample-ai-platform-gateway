## Para qué sirve esta pestaña
Plan y facturación muestra la medición del mes corriente (leída del historial real), la factura estimada y el cambio de plan. El gasto de LLM es tuyo (credencial propia); la plataforma cobra la suscripción y, en los planes que lo tienen, una fracción del ahorro comprobado.

## Cómo usarla
Mira las tarjetas de consumo y la factura estimada del mes. Para cambiar de plan, elige el nuevo tier — los límites (asientos, rate limit, budget) pasan a valer como config de tu organización.

## Preguntas frecuentes
- **¿La factura es un cobro real?** No en esta fase — es una estimación a partir del uso medido.
- **¿Qué cambia al subir de plan?** Asientos de miembros, límites y el modelo de acceso (por usuario en Pro, por equipo en Business).

## Por qué el saldo de crédito es estimado

El crédito de proveedor (AWS Activate, Google Cloud, Azure) es **saldo**, no descuento: el precio por token no cambia, cambia de qué bolsillo sale. Mientras haya saldo, el enrutamiento prefiere el proveedor cubierto — quemar el crédito antes de que expire es lo correcto.

El saldo que se muestra aquí es **estimado**, y es un **límite inferior** del consumo real. La razón es simple: solo contamos lo que pasó por el gateway, y el crédito del proveedor también se consume con todo lo que usas fuera de la plataforma (almacenamiento, máquinas, otros servicios).

**Qué hacer:** revisa el saldo en la factura del proveedor y corrige el valor en pantalla. La corrección manual pasa a ser la base del cálculo, en lugar del valor declarado originalmente.

Cuando el crédito expira, el gateway vuelve a decidir por dinero real y tu budget vuelve a valer normalmente.
